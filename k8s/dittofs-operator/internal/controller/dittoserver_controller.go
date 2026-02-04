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

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/internal/controller/config"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/pkg/resources"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/conditions"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/nfs"
	"github.com/marmos91/dittofs/k8s/dittofs-operator/utils/smb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
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
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
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

	// Reconcile services (headless required for StatefulSet DNS)
	if err := r.reconcileHeadlessService(ctx, dittoServer); err != nil {
		logger.Error(err, "Failed to reconcile headless Service")
		return ctrl.Result{}, err
	}

	if err := r.reconcileFileService(ctx, dittoServer); err != nil {
		logger.Error(err, "Failed to reconcile file Service")
		return ctrl.Result{}, err
	}

	if err := r.reconcileAPIService(ctx, dittoServer); err != nil {
		logger.Error(err, "Failed to reconcile API Service")
		return ctrl.Result{}, err
	}

	if err := r.reconcileMetricsService(ctx, dittoServer); err != nil {
		logger.Error(err, "Failed to reconcile metrics Service")
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

	statefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: dittoServer.Namespace,
		Name:      dittoServer.Name,
	}, statefulSet); err != nil {
		logger.Error(err, "Failed to get StatefulSet")
		return ctrl.Result{}, err
	}

	dittoServerCopy := dittoServer.DeepCopy()
	dittoServerCopy.Status.AvailableReplicas = statefulSet.Status.ReadyReplicas

	if replicas == 0 {
		dittoServerCopy.Status.Phase = "Stopped"
	} else if statefulSet.Status.ReadyReplicas == replicas {
		dittoServerCopy.Status.Phase = "Running"
	} else {
		dittoServerCopy.Status.Phase = "Pending"
	}

	dittoServerCopy.Status.NFSEndpoint = fmt.Sprintf("%s-file.%s.svc.cluster.local:%d",
		dittoServer.Name, dittoServer.Namespace, nfs.GetNFSPort(dittoServer))

	if statefulSet.Status.ReadyReplicas == replicas && replicas > 0 {
		conditions.SetCondition(&dittoServerCopy.Status.Conditions, dittoServer.Generation,
			"Ready", metav1.ConditionTrue, "StatefulSetReady",
			fmt.Sprintf("StatefulSet has %d/%d ready replicas", statefulSet.Status.ReadyReplicas, replicas))
	} else if replicas == 0 {
		conditions.SetCondition(&dittoServerCopy.Status.Conditions, dittoServer.Generation,
			"Ready", metav1.ConditionTrue, "Stopped", "DittoServer is stopped (replicas=0)")
	} else {
		conditions.SetCondition(&dittoServerCopy.Status.Conditions, dittoServer.Generation,
			"Ready", metav1.ConditionFalse, "StatefulSetNotReady",
			fmt.Sprintf("StatefulSet has %d/%d ready replicas", statefulSet.Status.ReadyReplicas, replicas))
	}

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

		configYAML, err := config.GenerateDittoFSConfig(ctx, r.Client, dittoServer)
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

// getServiceType returns the Kubernetes Service type from the CRD spec.
func getServiceType(dittoServer *dittoiov1alpha1.DittoServer) corev1.ServiceType {
	if dittoServer.Spec.Service.Type != "" {
		return corev1.ServiceType(dittoServer.Spec.Service.Type)
	}
	return corev1.ServiceTypeLoadBalancer // Default to LoadBalancer
}

// getAPIPort returns the control plane API port (default 8080).
func getAPIPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.ControlPlane != nil && dittoServer.Spec.ControlPlane.Port > 0 {
		return dittoServer.Spec.ControlPlane.Port
	}
	return 8080
}

// getMetricsPort returns the metrics port (default 9090).
func getMetricsPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.Metrics != nil && dittoServer.Spec.Metrics.Port > 0 {
		return dittoServer.Spec.Metrics.Port
	}
	return 9090
}

// reconcileHeadlessService creates/updates the headless Service for StatefulSet DNS.
func (r *DittoServerReconciler) reconcileHeadlessService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	labels := map[string]string{
		"app":      "dittofs-server",
		"instance": dittoServer.Name,
	}

	nfsPort := nfs.GetNFSPort(dittoServer)

	svc := resources.NewServiceBuilder(dittoServer.Name+"-headless", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		AsHeadless().
		AddTCPPort("nfs", nfsPort).
		Build()

	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: svc.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, existing, r.Scheme); err != nil {
			return err
		}
		existing.Spec = svc.Spec
		existing.Labels = svc.Labels
		return nil
	})

	return err
}

// reconcileFileService creates/updates the Service for file protocols (NFS, SMB).
func (r *DittoServerReconciler) reconcileFileService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	labels := map[string]string{
		"app":      "dittofs-server",
		"instance": dittoServer.Name,
	}

	nfsPort := nfs.GetNFSPort(dittoServer)

	builder := resources.NewServiceBuilder(dittoServer.Name+"-file", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		WithType(getServiceType(dittoServer)).
		WithAnnotations(dittoServer.Spec.Service.Annotations).
		AddTCPPort("nfs", nfsPort)

	// Add SMB port if enabled
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Enabled {
		smbPort := smb.GetSMBPort(dittoServer)
		builder.AddTCPPort("smb", smbPort)
	}

	svc := builder.Build()

	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: svc.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, existing, r.Scheme); err != nil {
			return err
		}
		existing.Spec = svc.Spec
		existing.Labels = svc.Labels
		existing.Annotations = svc.Annotations
		return nil
	})

	return err
}

// reconcileAPIService creates/updates the Service for REST API access.
func (r *DittoServerReconciler) reconcileAPIService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	labels := map[string]string{
		"app":      "dittofs-server",
		"instance": dittoServer.Name,
	}

	apiPort := getAPIPort(dittoServer)

	svc := resources.NewServiceBuilder(dittoServer.Name+"-api", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		WithType(getServiceType(dittoServer)).
		WithAnnotations(dittoServer.Spec.Service.Annotations).
		AddTCPPort("api", apiPort).
		Build()

	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: svc.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, existing, r.Scheme); err != nil {
			return err
		}
		existing.Spec = svc.Spec
		existing.Labels = svc.Labels
		existing.Annotations = svc.Annotations
		return nil
	})

	return err
}

// reconcileMetricsService creates/updates the Service for Prometheus metrics (if enabled).
func (r *DittoServerReconciler) reconcileMetricsService(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) error {
	// Only create metrics service if metrics are enabled
	if dittoServer.Spec.Metrics == nil || !dittoServer.Spec.Metrics.Enabled {
		// Delete metrics service if it exists
		existing := &corev1.Service{}
		err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Name + "-metrics",
		}, existing)
		if err == nil {
			// Service exists, delete it
			return r.Delete(ctx, existing)
		}
		return client.IgnoreNotFound(err)
	}

	labels := map[string]string{
		"app":      "dittofs-server",
		"instance": dittoServer.Name,
	}

	metricsPort := getMetricsPort(dittoServer)

	// Metrics service is always ClusterIP (internal only)
	svc := resources.NewServiceBuilder(dittoServer.Name+"-metrics", dittoServer.Namespace).
		WithLabels(labels).
		WithSelector(labels).
		WithType(corev1.ServiceTypeClusterIP).
		AddTCPPort("metrics", metricsPort).
		Build()

	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: svc.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		if err := controllerutil.SetControllerReference(dittoServer, existing, r.Scheme); err != nil {
			return err
		}
		existing.Spec = svc.Spec
		existing.Labels = svc.Labels
		return nil
	})

	return err
}

func (r *DittoServerReconciler) reconcileStatefulSet(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer, replicas int32) error {
	logger := logf.FromContext(ctx)

	// Generate config to compute hash (same config that was just written to ConfigMap)
	configYAML, err := config.GenerateDittoFSConfig(ctx, r.Client, dittoServer)
	if err != nil {
		return fmt.Errorf("failed to generate config for hash: %w", err)
	}

	// Collect secret data for hash computation
	secretData, err := r.collectSecretData(ctx, dittoServer)
	if err != nil {
		logger.Error(err, "Failed to collect secret data for hash, using config-only hash")
		secretData = make(map[string][]byte)
	}

	// Compute config hash BEFORE CreateOrUpdate
	configHash := resources.ComputeConfigHash(configYAML, secretData, dittoServer.Generation)

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dittoServer.Name,
			Namespace: dittoServer.Namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, statefulSet, func() error {
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
				Name:      "cache",
				MountPath: "/data/cache",
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

		metadataSize, err := resource.ParseQuantity(dittoServer.Spec.Storage.MetadataSize)
		if err != nil {
			return fmt.Errorf("invalid metadata size: %w", err)
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
							corev1.ResourceStorage: metadataSize,
						},
					},
				},
			},
		}

		if dittoServer.Spec.Storage.ContentSize != "" {
			contentSize, err := resource.ParseQuantity(dittoServer.Spec.Storage.ContentSize)
			if err != nil {
				return fmt.Errorf("invalid content size: %w", err)
			}

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
							corev1.ResourceStorage: contentSize,
						},
					},
				},
			})
		}

		statefulSet.Spec = appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: dittoServer.Name + "-headless",
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						resources.ConfigHashAnnotation: configHash,
					},
				},
				Spec: corev1.PodSpec{
					SecurityContext: getPodSecurityContext(dittoServer),
					Containers: []corev1.Container{
						{
							Name:            "dittofs",
							Image:           dittoServer.Spec.Image,
							Command:         []string{"/app/dittofs"},
							Args:            []string{"start", "--config", "/config/config.yaml"},
							VolumeMounts:    volumeMounts,
							Resources:       dittoServer.Spec.Resources,
							SecurityContext: dittoServer.Spec.SecurityContext,
							Ports:           buildContainerPorts(dittoServer),
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt32(nfs.GetNFSPort(dittoServer)),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								SuccessThreshold:    1,
								FailureThreshold:    3,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt32(nfs.GetNFSPort(dittoServer)),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       5,
								TimeoutSeconds:      3,
								SuccessThreshold:    1,
								FailureThreshold:    3,
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
						{
							Name: "cache",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
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

// getPodSecurityContext returns the pod security context with fsGroup set to ensure
// that mounted volumes are writable by the container user (65532)
func getPodSecurityContext(dittoServer *dittoiov1alpha1.DittoServer) *corev1.PodSecurityContext {
	if dittoServer.Spec.PodSecurityContext != nil {
		return dittoServer.Spec.PodSecurityContext
	}

	// Default security context with fsGroup to nonroot user (65532)
	fsGroup := int64(65532)
	return &corev1.PodSecurityContext{
		FSGroup: &fsGroup,
	}
}

// buildContainerPorts constructs the container ports based on enabled protocols
func buildContainerPorts(dittoServer *dittoiov1alpha1.DittoServer) []corev1.ContainerPort {
	nfsPort := nfs.GetNFSPort(dittoServer)

	ports := []corev1.ContainerPort{
		{
			Name:          "nfs",
			ContainerPort: nfsPort,
			Protocol:      corev1.ProtocolTCP,
		},
	}

	// Add API port
	apiPort := getAPIPort(dittoServer)
	ports = append(ports, corev1.ContainerPort{
		Name:          "api",
		ContainerPort: apiPort,
		Protocol:      corev1.ProtocolTCP,
	})

	// Add metrics port if enabled
	if dittoServer.Spec.Metrics != nil && dittoServer.Spec.Metrics.Enabled {
		metricsPort := getMetricsPort(dittoServer)
		ports = append(ports, corev1.ContainerPort{
			Name:          "metrics",
			ContainerPort: metricsPort,
			Protocol:      corev1.ProtocolTCP,
		})
	}

	// Add SMB port if enabled
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Enabled {
		smbPort := smb.GetSMBPort(dittoServer)
		ports = append(ports, corev1.ContainerPort{
			Name:          "smb",
			ContainerPort: smbPort,
			Protocol:      corev1.ProtocolTCP,
		})
	}

	return ports
}

// collectSecretData gathers all secret data referenced by the DittoServer CR.
// This is used to compute the config hash - when any secret changes, the hash changes.
func (r *DittoServerReconciler) collectSecretData(ctx context.Context, dittoServer *dittoiov1alpha1.DittoServer) (map[string][]byte, error) {
	secrets := make(map[string][]byte)

	// JWT secret
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.JWT != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Spec.Identity.JWT.SecretRef.Name,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get JWT secret: %w", err)
		}
		key := dittoServer.Spec.Identity.JWT.SecretRef.Key
		if data, ok := secret.Data[key]; ok {
			secrets["jwt:"+key] = data
		}
	}

	// Admin password secret
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.Admin != nil &&
		dittoServer.Spec.Identity.Admin.PasswordSecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Spec.Identity.Admin.PasswordSecretRef.Name,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get admin password secret: %w", err)
		}
		key := dittoServer.Spec.Identity.Admin.PasswordSecretRef.Key
		if data, ok := secret.Data[key]; ok {
			secrets["admin:"+key] = data
		}
	}

	// PostgreSQL connection string secret (if configured)
	if dittoServer.Spec.Database != nil && dittoServer.Spec.Database.PostgresSecretRef != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: dittoServer.Namespace,
			Name:      dittoServer.Spec.Database.PostgresSecretRef.Name,
		}, secret); err != nil {
			return nil, fmt.Errorf("failed to get postgres secret: %w", err)
		}
		key := dittoServer.Spec.Database.PostgresSecretRef.Key
		if data, ok := secret.Data[key]; ok {
			secrets["postgres:"+key] = data
		}
	}

	return secrets, nil
}
