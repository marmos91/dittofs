package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewDittoServerSpec returns a DittoServerSpec object with the given options
func NewDittoServerSpec(opts ...func(*DittoServerSpec)) *DittoServerSpec {
	obj := &DittoServerSpec{}

	for _, f := range opts {
		f(obj)
	}

	return obj
}

// WithImage sets the Image of a DittoServerSpec
func WithImage(image string) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Image = image
	}
}

// WithReplicas sets the Replicas of a DittoServerSpec
func WithReplicas(replicas *int32) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Replicas = replicas
	}
}

// WithStorage sets the Storage of a DittoServerSpec
func WithStorage(storage StorageSpec) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Storage = storage
	}
}

// WithDatabase sets the Database of a DittoServerSpec
func WithDatabase(database *DatabaseConfig) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Database = database
	}
}

// WithCache sets the Cache of a DittoServerSpec
func WithCache(cache *InfraCacheConfig) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Cache = cache
	}
}

// WithMetrics sets the Metrics of a DittoServerSpec
func WithMetrics(metrics *MetricsConfig) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Metrics = metrics
	}
}

// WithControlPlane sets the ControlPlane of a DittoServerSpec
func WithControlPlane(controlPlane *ControlPlaneAPIConfig) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.ControlPlane = controlPlane
	}
}

// WithIdentity sets the Identity of a DittoServerSpec
func WithIdentity(identity *IdentityConfig) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Identity = identity
	}
}

// WithAdapterDiscovery sets the AdapterDiscovery of a DittoServerSpec
func WithAdapterDiscovery(ad *AdapterDiscoverySpec) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.AdapterDiscovery = ad
	}
}

// WithAdapterServices sets the AdapterServices of a DittoServerSpec
func WithAdapterServices(as *AdapterServiceConfig) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.AdapterServices = as
	}
}

// WithService sets the Service of a DittoServerSpec
func WithService(service ServiceSpec) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Service = service
	}
}

// WithResources sets the Resources of a DittoServerSpec
func WithResources(resources corev1.ResourceRequirements) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.Resources = resources
	}
}

// WithSecurityContext sets the SecurityContext of a DittoServerSpec
func WithSecurityContext(securitycontext *corev1.SecurityContext) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.SecurityContext = securitycontext
	}
}

// WithPodSecurityContext sets the PodSecurityContext of a DittoServerSpec
func WithPodSecurityContext(podsecuritycontext *corev1.PodSecurityContext) func(*DittoServerSpec) {
	return func(obj *DittoServerSpec) {
		obj.PodSecurityContext = podsecuritycontext
	}
}

// NewDittoServerStatus returns a DittoServerStatus object with the given options
func NewDittoServerStatus(opts ...func(*DittoServerStatus)) *DittoServerStatus {
	obj := &DittoServerStatus{}

	for _, f := range opts {
		f(obj)
	}

	return obj
}

// WithAvailableReplicas sets the AvailableReplicas of a DittoServerStatus
func WithAvailableReplicas(availablereplicas int32) func(*DittoServerStatus) {
	return func(obj *DittoServerStatus) {
		obj.AvailableReplicas = availablereplicas
	}
}

// WithPhase sets the Phase of a DittoServerStatus
func WithPhase(phase string) func(*DittoServerStatus) {
	return func(obj *DittoServerStatus) {
		obj.Phase = phase
	}
}

// WithConditions sets the Conditions of a DittoServerStatus
func WithConditions(conditions []metav1.Condition) func(*DittoServerStatus) {
	return func(obj *DittoServerStatus) {
		obj.Conditions = conditions
	}
}

// NewDittoServer returns a DittoServer object with the given options
func NewDittoServer(opts ...func(*DittoServer)) *DittoServer {
	obj := &DittoServer{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DittoServer",
			APIVersion: "dittofs.dittofs.com/v1alpha1",
		},
	}

	for _, f := range opts {
		f(obj)
	}

	return obj
}

// WithName sets the name of the DittoServer
func WithName(name string) func(*DittoServer) {
	return func(obj *DittoServer) {
		obj.Name = name
	}
}

// WithNamespace sets the namespace of the DittoServer
func WithNamespace(namespace string) func(*DittoServer) {
	return func(obj *DittoServer) {
		obj.Namespace = namespace
	}
}

// WithLabel sets a label of the DittoServer
func WithLabel(k, v string) func(*DittoServer) {
	return func(obj *DittoServer) {
		if obj.Labels == nil {
			obj.Labels = make(map[string]string)
		}
		obj.Labels[k] = v
	}
}

// WithAnnotation sets an annotation of the DittoServer
func WithAnnotation(k, v string) func(*DittoServer) {
	return func(obj *DittoServer) {
		if obj.Annotations == nil {
			obj.Annotations = make(map[string]string)
		}
		obj.Annotations[k] = v
	}
}

// WithFinalizer sets the finalizers of the DittoServer
func WithFinalizer(f string) func(*DittoServer) {
	return func(obj *DittoServer) {
		obj.Finalizers = append(obj.Finalizers, f)
	}
}

// WithCreationTimestamp sets the deletion timestamp of the DittoServer
func WithCreationTimestamp(timestamp metav1.Time) func(*DittoServer) {
	return func(obj *DittoServer) {
		obj.CreationTimestamp = timestamp
	}
}

// WithDeletionTimestamp sets the deletion timestamp of the DittoServer
func WithDeletionTimestamp(timestamp *metav1.Time) func(*DittoServer) {
	return func(obj *DittoServer) {
		obj.DeletionTimestamp = timestamp
	}
}

// WithSpec sets the Spec of a DittoServer
func WithSpec(spec DittoServerSpec) func(*DittoServer) {
	return func(obj *DittoServer) {
		obj.Spec = spec
	}
}

// WithStatus sets the Status of a DittoServer
func WithStatus(status DittoServerStatus) func(*DittoServer) {
	return func(obj *DittoServer) {
		obj.Status = status
	}
}
