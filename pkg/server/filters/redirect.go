package filters

import (
	"fmt"
	"github.com/loft-sh/vcluster/pkg/authorization/delegatingauthorizer"
	"github.com/loft-sh/vcluster/pkg/server/handler"
	requestpkg "github.com/loft-sh/vcluster/pkg/util/request"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"strings"
)

func WithRedirect(h http.Handler, localManager ctrl.Manager, virtualManager ctrl.Manager, admit admission.Interface, targetNamespace string, resources []delegatingauthorizer.GroupVersionResourceVerb) http.Handler {
	s := serializer.NewCodecFactory(localManager.GetScheme())
	parameterCodec := runtime.NewParameterCodec(virtualManager.GetScheme())
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		info, ok := request.RequestInfoFrom(req.Context())
		if !ok {
			requestpkg.FailWithStatus(w, req, http.StatusInternalServerError, fmt.Errorf("request info is missing"))
			return
		}

		if applies(info, resources) {
			// call admission webhooks
			err := callAdmissionWebhooks(req, info, parameterCodec, admit, virtualManager)
			if err != nil {
				responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
				return
			}

			// we have to change the request url
			if info.Resource != "nodes" {
				if info.Namespace == "" {
					responsewriters.ErrorNegotiated(kerrors.NewBadRequest("namespace required"), s, corev1.SchemeGroupVersion, w, req)
					return
				}

				splitted := strings.Split(req.URL.Path, "/")
				if len(splitted) < 6 {
					responsewriters.ErrorNegotiated(kerrors.NewBadRequest("unexpected url"), s, corev1.SchemeGroupVersion, w, req)
					return
				}

				// exchange namespace & name
				splitted[4] = targetNamespace

				// make sure we keep the prefix and suffix
				targetName := translate.PhysicalName(splitted[6], info.Namespace)
				splitted[6] = targetName
				req.URL.Path = strings.Join(splitted, "/")

				// we have to add a trailing slash here, because otherwise the
				// host api server would redirect us to a wrong path
				if len(splitted) == 8 {
					req.URL.Path += "/"
				}
			}

			h, err := handler.Handler("", localManager.GetConfig(), nil)
			if err != nil {
				requestpkg.FailWithStatus(w, req, http.StatusInternalServerError, err)
				return
			}

			req.Header.Del("Authorization")
			h.ServeHTTP(w, req)
			return
		}

		h.ServeHTTP(w, req)
	})
}

func callAdmissionWebhooks(req *http.Request, info *request.RequestInfo, parameterCodec runtime.ParameterCodec, admit admission.Interface, virtualManager ctrl.Manager) error {
	if info.Resource != "pods" {
		return nil
	} else if info.Subresource != "exec" && info.Subresource != "portforward" && info.Subresource != "attach" {
		return nil
	}

	if admit != nil && admit.Handles(admission.Connect) {
		userInfo, _ := request.UserFrom(req.Context())
		if validatingAdmission, ok := admit.(admission.ValidationInterface); ok {
			var opts runtime.Object
			var kind schema.GroupVersionKind
			if info.Subresource == "exec" {
				kind = corev1.SchemeGroupVersion.WithKind("PodExecOptions")
				opts = &corev1.PodExecOptions{}
				if err := parameterCodec.DecodeParameters(req.URL.Query(), corev1.SchemeGroupVersion, opts); err != nil {
					return err
				}
			} else if info.Subresource == "attach" {
				kind = corev1.SchemeGroupVersion.WithKind("PodAttachOptions")
				opts = &corev1.PodAttachOptions{}
				if err := parameterCodec.DecodeParameters(req.URL.Query(), corev1.SchemeGroupVersion, opts); err != nil {
					return err
				}
			} else if info.Subresource == "portforward" {
				kind = corev1.SchemeGroupVersion.WithKind("PodPortForwardOptions")
				opts = &corev1.PodPortForwardOptions{}
				if err := parameterCodec.DecodeParameters(req.URL.Query(), corev1.SchemeGroupVersion, opts); err != nil {
					return err
				}
			}

			err := validatingAdmission.Validate(req.Context(), admission.NewAttributesRecord(opts, nil, kind, info.Namespace, info.Name, corev1.SchemeGroupVersion.WithResource(info.Resource), info.Subresource, admission.Connect, nil, false, userInfo), NewFakeObjectInterfaces(virtualManager.GetScheme(), virtualManager.GetRESTMapper()))
			if err != nil {
				klog.Infof("Admission validate failed for %s: %v", info.Path, err)
				return err
			}
		}
	}

	return nil
}

func applies(r *request.RequestInfo, resources []delegatingauthorizer.GroupVersionResourceVerb) bool {
	if r.IsResourceRequest == false {
		return false
	}

	for _, gv := range resources {
		if (gv.Group == "*" || gv.Group == r.APIGroup) && (gv.Version == "*" || gv.Version == r.APIVersion) && (gv.Resource == "*" || gv.Resource == r.Resource) && (gv.Verb == "*" || gv.Verb == r.Verb) && (gv.SubResource == "*" || gv.SubResource == r.Subresource) {
			return true
		}
	}

	return false
}
