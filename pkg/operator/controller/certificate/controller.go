// The certificate controller is responsible for:
//
//   1. Managing a CA for minting self-signed certs
//   2. Managing self-signed certificates for any ingresscontrollers which require them
//   3. Publishing the CA to `openshift-config-managed`
package certificate

import (
	"context"
	"fmt"
	"time"

	logf "github.com/openshift/cluster-ingress-operator/pkg/log"
	"github.com/openshift/cluster-ingress-operator/pkg/operator/controller"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"k8s.io/client-go/tools/record"

	appsv1 "k8s.io/api/apps/v1"

	operatorv1 "github.com/openshift/api/operator/v1"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimecontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	controllerName = "certificate_controller"
)

var log = logf.Logger.WithName(controllerName)

func New(mgr manager.Manager, operatorNamespace string) (runtimecontroller.Controller, error) {
	reconciler := &reconciler{
		client:            mgr.GetClient(),
		cache:             mgr.GetCache(),
		recorder:          mgr.GetEventRecorderFor(controllerName),
		operatorNamespace: operatorNamespace,
	}
	c, err := runtimecontroller.New(controllerName, mgr, runtimecontroller.Options{Reconciler: reconciler})
	if err != nil {
		return nil, err
	}
	if err := c.Watch(&source.Kind{Type: &operatorv1.IngressController{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return nil, err
	}
	return c, nil
}

type reconciler struct {
	client            client.Client
	cache             cache.Cache
	recorder          record.EventRecorder
	operatorNamespace string
}

func (r *reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	ca, err := r.ensureRouterCASecret()
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to ensure router CA: %v", err)
	}

	result := reconcile.Result{}
	errs := []error{}
	ingress := &operatorv1.IngressController{}
	if err := r.client.Get(context.TODO(), request.NamespacedName, ingress); err != nil {
		if errors.IsNotFound(err) {
			// The ingress could have been deleted and we're processing a stale queue
			// item, so ignore and skip.
			log.Info("ingresscontroller not found; reconciliation will be skipped", "request", request)
		} else {
			errs = append(errs, fmt.Errorf("failed to get ingresscontroller: %v", err))
		}
	} else if !controller.IsStatusDomainSet(ingress) {
		log.Info("ingresscontroller domain not set; reconciliation will be skipped", "request", request)
	} else {
		deployment := &appsv1.Deployment{}
		err = r.client.Get(context.TODO(), controller.RouterDeploymentName(ingress), deployment)
		if err != nil {
			if errors.IsNotFound(err) {
				// All ingresses should have a deployment, so this one may not have been
				// created yet. Retry after a reasonable amount of time.
				log.Info("deployment not found; will retry default cert sync", "ingresscontroller", ingress.Name)
				result.RequeueAfter = 5 * time.Second
			} else {
				errs = append(errs, fmt.Errorf("failed to get deployment: %v", err))
			}
		} else {
			trueVar := true
			deploymentRef := metav1.OwnerReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployment.Name,
				UID:        deployment.UID,
				Controller: &trueVar,
			}
			if _, err := r.ensureDefaultCertificateForIngress(ca, deployment.Namespace, deploymentRef, ingress); err != nil {
				errs = append(errs, fmt.Errorf("failed to ensure default cert for %s: %v", ingress.Name, err))
			}
		}
	}

	ingresses := &operatorv1.IngressControllerList{}
	if err := r.cache.List(context.TODO(), ingresses, client.InNamespace(r.operatorNamespace)); err != nil {
		errs = append(errs, fmt.Errorf("failed to list ingresscontrollers: %v", err))
	} else if err := r.ensureRouterCAConfigMap(ca, ingresses.Items); err != nil {
		errs = append(errs, fmt.Errorf("failed to publish router CA: %v", err))
	}

	return result, utilerrors.NewAggregate(errs)
}
