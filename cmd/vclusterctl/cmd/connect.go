package cmd

import (
	"context"
	"fmt"
	"github.com/loft-sh/vcluster/pkg/upgrade"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/loft-sh/vcluster/cmd/vclusterctl/flags"
	"github.com/loft-sh/vcluster/cmd/vclusterctl/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// ConnectCmd holds the login cmd flags
type ConnectCmd struct {
	*flags.GlobalFlags

	KubeConfig    string
	PodName       string
	UpdateCurrent bool
	Print         bool
	LocalPort     int
	Address       string

	Server string

	log log.Logger
}

// NewConnectCmd creates a new command
func NewConnectCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &ConnectCmd{
		GlobalFlags: globalFlags,
		log:         log.GetInstance(),
	}

	cobraCmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect to a virtual cluster",
		Long: `
#######################################################
################## vcluster connect ###################
#######################################################
Connect to a virtual cluster

Example:
vcluster connect test --namespace test
#######################################################
	`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			// Check for newer version
			upgrade.PrintNewerVersionWarning()

			return cmd.Run(cobraCmd, args)
		},
	}

	cobraCmd.Flags().StringVar(&cmd.KubeConfig, "kube-config", "./kubeconfig.yaml", "Writes the created kube config to this file")
	cobraCmd.Flags().BoolVar(&cmd.UpdateCurrent, "update-current", false, "If true updates the current kube config")
	cobraCmd.Flags().BoolVar(&cmd.Print, "print", false, "When enabled prints the context to stdout")
	cobraCmd.Flags().StringVar(&cmd.PodName, "pod", "", "The pod to connect to")
	cobraCmd.Flags().StringVar(&cmd.Server, "server", "", "The server to connect to")
	cobraCmd.Flags().IntVar(&cmd.LocalPort, "local-port", 8443, "The local port to forward the virtual cluster to")
	cobraCmd.Flags().StringVar(&cmd.Address, "address", "", "The local address to start port forwarding under")
	return cobraCmd
}

// Run executes the functionality
func (cmd *ConnectCmd) Run(cobraCmd *cobra.Command, args []string) error {
	vclusterName := ""
	if len(args) > 0 {
		vclusterName = args[0]
	}

	return cmd.Connect(vclusterName)
}

func (cmd *ConnectCmd) Connect(vclusterName string) error {
	kubeConfigLoader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{
		CurrentContext: cmd.Context,
	})

	// set the namespace correctly
	var err error
	if cmd.Namespace == "" {
		cmd.Namespace, _, err = kubeConfigLoader.Namespace()
		if err != nil {
			return err
		}
	}

	if vclusterName == "" && cmd.PodName == "" {
		return fmt.Errorf("please specify either --pod or a name for the vcluster")
	}

	podName := cmd.PodName
	if podName == "" {
		podName = vclusterName + "-0"
	}

	// get the kube config from the container
	var out []byte
	printedWaiting := false
	err = wait.PollImmediate(time.Second*2, time.Minute*5, func() (done bool, err error) {
		args := []string{"exec", "--namespace", cmd.Namespace, "-c", "syncer", podName, "--", "cat", "/data/.kube/config"}
		if cmd.Context != "" {
			newArgs := []string{"--context", cmd.Context}
			newArgs = append(newArgs, args...)
			args = newArgs
		}

		out, err = exec.Command("kubectl", args...).CombinedOutput()
		if err != nil {
			if !printedWaiting {
				cmd.log.Infof("Waiting for vcluster to come up...")
				printedWaiting = true
			}

			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return errors.Wrap(err, "wait for vcluster")
	}

	kubeConfig, err := clientcmd.Load(out)
	if err != nil {
		return errors.Wrap(err, "parse kube config")
	}

	// find out port we should listen to locally
	if len(kubeConfig.Clusters) != 1 {
		return fmt.Errorf("unexpected kube config")
	}

	// check if the vcluster is exposed
	if vclusterName != "" && cmd.Server == "" {
		restConfig, err := kubeConfigLoader.ClientConfig()
		if err != nil {
			return errors.Wrap(err, "load kube config")
		}
		kubeClient, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return errors.Wrap(err, "create kube client")
		}

		printedWaiting := false
		err = wait.PollImmediate(time.Second*2, time.Minute*5, func() (done bool, err error) {
			service, err := kubeClient.CoreV1().Services(cmd.Namespace).Get(context.TODO(), vclusterName, metav1.GetOptions{})
			if err != nil {
				if kerrors.IsNotFound(err) {
					return true, nil
				}

				return false, err
			}

			// not a load balancer? Then don't wait
			if service.Spec.Type != corev1.ServiceTypeLoadBalancer {
				return true, nil
			}

			if len(service.Status.LoadBalancer.Ingress) == 0 {
				if !printedWaiting {
					cmd.log.Infof("Waiting for vcluster LoadBalancer ip...")
					printedWaiting = true
				}

				return false, nil
			}

			if service.Status.LoadBalancer.Ingress[0].Hostname != "" {
				cmd.Server = service.Status.LoadBalancer.Ingress[0].Hostname
			} else if service.Status.LoadBalancer.Ingress[0].IP != "" {
				cmd.Server = service.Status.LoadBalancer.Ingress[0].IP
			}

			if cmd.Server == "" {
				return false, nil
			}

			cmd.log.Infof("Using vcluster %s load balancer endpoint: %s", vclusterName, cmd.Server)
			return true, nil
		})
		if err != nil {
			return errors.Wrap(err, "wait for vcluster")
		}
	}

	port := ""
	for k := range kubeConfig.Clusters {
		if cmd.Server != "" {
			if strings.HasPrefix(cmd.Server, "https://") == false {
				cmd.Server = "https://" + cmd.Server
			}

			kubeConfig.Clusters[k].Server = cmd.Server
		} else {
			splitted := strings.Split(kubeConfig.Clusters[k].Server, ":")
			if len(splitted) != 3 {
				return fmt.Errorf("unexpected server in kubeconfig: %s", kubeConfig.Clusters[k].Server)
			}

			port = splitted[2]
			splitted[2] = strconv.Itoa(cmd.LocalPort)
			kubeConfig.Clusters[k].Server = strings.Join(splitted, ":")
		}
	}

	out, err = clientcmd.Write(*kubeConfig)
	if err != nil {
		return err
	}

	// write kube config to file
	if cmd.UpdateCurrent {
		var clusterConfig *api.Cluster
		for _, c := range kubeConfig.Clusters {
			clusterConfig = c
		}

		var authConfig *api.AuthInfo
		for _, a := range kubeConfig.AuthInfos {
			authConfig = a
		}

		contextName := ""
		if vclusterName != "" {
			contextName = "vcluster_" + cmd.Namespace + "_" + vclusterName
		} else {
			contextName = "vcluster_" + cmd.Namespace + "_" + cmd.PodName
		}
		err = updateKubeConfig(contextName, clusterConfig, authConfig, false)
		if err != nil {
			return err
		}

		cmd.log.Donef("Successfully created kube context %s. You can access the vcluster with `kubectl get namespaces --context %s`", contextName, contextName)
	} else if cmd.Print {
		_, err = os.Stdout.Write(out)
		if err != nil {
			return err
		}
	} else {
		err = ioutil.WriteFile(cmd.KubeConfig, out, 0666)
		if err != nil {
			return errors.Wrap(err, "write kube config")
		}

		cmd.log.Donef("Virtual cluster kube config written to: %s. You can access the cluster via `kubectl --kubeconfig %s get namespaces`", cmd.KubeConfig, cmd.KubeConfig)
	}

	if cmd.Server != "" {
		return nil
	}

	forwardPorts := strconv.Itoa(cmd.LocalPort) + ":" + port
	command := []string{"kubectl", "port-forward", "--namespace", cmd.Namespace, podName, forwardPorts}
	if cmd.Context != "" {
		command = append(command, "--context", cmd.Context)
	}
	if cmd.Address != "" {
		command = append(command, "--address", cmd.Address)
	}

	cmd.log.Infof("Starting port forwarding: %s", strings.Join(command, " "))
	portforwardCmd := exec.Command(command[0], command[1:]...)
	if !cmd.Print {
		portforwardCmd.Stdout = os.Stdout
	} else {
		portforwardCmd.Stdout = ioutil.Discard
	}

	portforwardCmd.Stderr = os.Stderr
	return portforwardCmd.Run()
}

func updateKubeConfig(contextName string, cluster *api.Cluster, authInfo *api.AuthInfo, setActive bool) error {
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return err
	}

	config.Clusters[contextName] = cluster
	config.AuthInfos[contextName] = authInfo

	// Update kube context
	context := api.NewContext()
	context.Cluster = contextName
	context.AuthInfo = contextName

	config.Contexts[contextName] = context
	if setActive {
		config.CurrentContext = contextName
	}

	// Save the config
	return clientcmd.ModifyConfig(clientcmd.NewDefaultClientConfigLoadingRules(), config, false)
}
