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

	dittoiov1alpha1 "github.com/marmos91/dittofs/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/dittofs-operator/internal/controller/config"
	"github.com/marmos91/dittofs/dittofs-operator/utils/conditions"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	logger := logf.FromContext(ctx)

	dittoServer := &dittoiov1alpha1.DittoServer{}
	if err := r.Get(ctx, req.NamespacedName, dittoServer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.reconcileConfigMap(ctx, dittoServer); err != nil {
		logger.Error(err, "Failed to reconcile ConfigMap")
		return ctrl.Result{}, err
	}

	if err := r.reconcileService(ctx, dittoServer); err != nil {
		logger.Error(err, "Failed to reconcile Service")
		return ctrl.Result{}, err
	}

	replicas := int32(1)
	if dittoServer.Spec.Replicas != nil {
		replicas = *dittoServer.Spec.Replicas
	}

	if err := r.reconcileStatefulSet(ctx, dittoServer, replicas); err != nil {
		logger.Error(err, "Failed to reconcile StatefulSet")
		return ctrl.Result{}, err
	}

	dittoServerCopy := dittoServer.DeepCopy()
	if replicas == 0 {
		dittoServerCopy.Status.Phase = "Stopped"
	} else {
		dittoServerCopy.Status.Phase = "Running"
	}

	dittoServerCopy.Status.NFSEndpoint = fmt.Sprintf("%s.%s.svc.cluster.local:2049",
		dittoServer.Name, dittoServer.Namespace)

	conditions.SetCondition(&dittoServerCopy.Status.Conditions, dittoServer.Generation,
		"Ready", metav1.ConditionTrue, "ReconcileSuccess", "All resources reconciled successfully")

	if err := r.Status().Update(ctx, dittoServerCopy); err != nil {
		logger.Error(err, "Failed to update DittoServer status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DittoServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dittoiov1alpha1.DittoServer{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
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

func (r *DittoServerReconciler) reconcileService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dittoServer.Name,
			Namespace: dittoServer.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, service, r.Scheme); err != nil {
			return err
		}

		serviceType := corev1.ServiceTypeClusterIP
		if dittoServer.Spec.Service.Type != "" {
			serviceType = corev1.ServiceType(dittoServer.Spec.Service.Type)
		}

		service.Spec = corev1.ServiceSpec{
			Type: serviceType,
			Selector: map[string]string{
				"app":      "dittofs-server",
				"instance": dittoServer.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "nfs",
					Port:     2049,
					Protocol: corev1.ProtocolTCP,
				},
				{
					Name:     "metrics",
					Port:     9090,
					Protocol: corev1.ProtocolTCP,
				},
			},
		}

		if dittoServer.Spec.Service.Annotations != nil {
			service.Annotations = dittoServer.Spec.Service.Annotations
		}

		return nil
	})

	return err
}

func (r *DittoServerReconciler) reconcileStatefulSet(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer, replicas int32) error {
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dittoServer.Name,
			Namespace: dittoServer.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, statefulSet, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, statefulSet, r.Scheme); err != nil {
			return err
		}

		labels := map[string]string{
			"app":      "dittofs-server",
			"instance": dittoServer.Name,
		}

		volumeMounts := []corev1.VolumeMount{
			{
				Name:      "metadata",
				MountPath: "/data/metadata",
			},
			{
				Name:      "config",
				MountPath: "/config",
			},
		}

		if dittoServer.Spec.Storage.ContentSize != "" {
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      "content",
				MountPath: "/data/content",
			})
		}

		volumeClaimTemplates := []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "metadata",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
					},
					StorageClassName: dittoServer.Spec.Storage.StorageClassName,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(dittoServer.Spec.Storage.MetadataSize),
						},
					},
				},
			},
		}

		if dittoServer.Spec.Storage.ContentSize != "" {
			volumeClaimTemplates = append(volumeClaimTemplates, corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "content",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
					},
					StorageClassName: dittoServer.Spec.Storage.StorageClassName,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(dittoServer.Spec.Storage.ContentSize),
						},
					},
				},
			})
		}

		statefulSet.Spec = appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: dittoServer.Name,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:         "dittofs",
							Image:        dittoServer.Spec.Image,
							Command:      []string{"/app/dittofs"},
							Args:         []string{"start", "--config", "/config/config.yaml"},
							VolumeMounts: volumeMounts,
							Ports: []corev1.ContainerPort{
								{
									Name:          "nfs",
									ContainerPort: 2049,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "metrics",
									ContainerPort: 9090,
									Protocol:      corev1.ProtocolTCP,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: dittoServer.Name + "-config",
									},
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: volumeClaimTemplates,
		}

		return nil
	})

	return err
}
