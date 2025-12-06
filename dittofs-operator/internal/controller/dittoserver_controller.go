/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"github.com/labstack/gommon/log"
	dittoiov1alpha1 "github.com/marmos91/dittofs/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/dittofs-operator/internal/controller/config"
	"github.com/marmos91/dittofs/dittofs-operator/utils/conditions"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DittoServerReconciler reconciles a DittoServer object
type DittoServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dittofs.dittofs.com,resources=dittoservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DittoServer object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *DittoServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	dittoServer := &dittoiov1alpha1.DittoServer{}
	if err := r.Get(ctx, req.NamespacedName, dittoServer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// replicas := int32(1)
	// if dittoServer.Spec.Replicas != nil {
	// 	replicas = *dittoServer.Spec.Replicas
	// }

	// if replicas == 0 {
	// 	dittoServer.Status.Phase = "Stopped"
	// } else {
	// 	dittoServer.Status.Phase = "Running"
	// }

	// Reconcile ConfigMap
	if err := r.reconcileConfigMap(ctx, dittoServer); err != nil {
		log.Error(err, "Failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}

	dittoServerCopy := dittoServer.DeepCopy()
	dittoServerCopy.Status.Phase = "Pending"
	conditions.SetCondition(&dittoServerCopy.Status.Conditions, dittoServer.Generation, "Creating", metav1.ConditionTrue, "GeneratedConfigMap", "ConfigMap has been generated")

	if err := r.Status().Update(ctx, dittoServerCopy); err != nil {
		log.Error(err, "Failed to update DittoServer status")
		return ctrl.Result{}, err
	}

	// to be done:
	// 2. reconcile pvc for badger/postgres
	// 3. reconcile pvc for content (eventually)
	// 4. reconcile service to expose the server (get annotation)
	// 5. reconcile statefulset of dittofs operator
	//		5.1 pass all informations
	// if err := r.reconcileStatefulSet(ctx, dittoServer, replicas); err != nil {
	// 	return ctrl.Result{}, err
	// }

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DittoServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dittoiov1alpha1.DittoServer{}).
		Named("dittoserver").
		Complete(r)
}

func (r *DittoServerReconciler) reconcileConfigMap(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dittoServer.Name + "-config",
			Namespace: dittoServer.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, configMap, r.Scheme); err != nil {
			return err
		}

		configYAML, err := config.GenerateDittoFSConfig(dittoServer)
		if err != nil {
			return fmt.Errorf("failed to generate config: %w", err)
		}

		configMap.Data = map[string]string{
			"config.yaml": configYAML,
		}

		return nil
	})

	return err
}
