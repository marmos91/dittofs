// Package resources provides utilities for building Kubernetes resources.
package resources

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ServiceBuilder provides a fluent API for building Kubernetes Services.
type ServiceBuilder struct {
	name        string
	namespace   string
	labels      map[string]string
	selector    map[string]string
	annotations map[string]string
	serviceType corev1.ServiceType
	ports       []corev1.ServicePort
	headless    bool
}

// NewServiceBuilder creates a new ServiceBuilder with ClusterIP as default.
// Use WithType() to explicitly set LoadBalancer or NodePort if needed.
func NewServiceBuilder(name, namespace string) *ServiceBuilder {
	return &ServiceBuilder{
		name:        name,
		namespace:   namespace,
		serviceType: corev1.ServiceTypeClusterIP,
		labels:      make(map[string]string),
		selector:    make(map[string]string),
		annotations: make(map[string]string),
	}
}

// WithLabels sets the labels for the Service.
func (b *ServiceBuilder) WithLabels(labels map[string]string) *ServiceBuilder {
	for k, v := range labels {
		b.labels[k] = v
	}
	return b
}

// WithSelector sets the selector for the Service.
func (b *ServiceBuilder) WithSelector(selector map[string]string) *ServiceBuilder {
	for k, v := range selector {
		b.selector[k] = v
	}
	return b
}

// WithAnnotations sets annotations for the Service (e.g., cloud provider LB config).
func (b *ServiceBuilder) WithAnnotations(annotations map[string]string) *ServiceBuilder {
	for k, v := range annotations {
		b.annotations[k] = v
	}
	return b
}

// WithType sets the Service type (ClusterIP, NodePort, LoadBalancer).
func (b *ServiceBuilder) WithType(t corev1.ServiceType) *ServiceBuilder {
	b.serviceType = t
	return b
}

// AsHeadless makes this a headless Service (ClusterIP: None).
func (b *ServiceBuilder) AsHeadless() *ServiceBuilder {
	b.headless = true
	b.serviceType = corev1.ServiceTypeClusterIP
	return b
}

// AddTCPPort adds a TCP port to the Service.
func (b *ServiceBuilder) AddTCPPort(name string, port int32) *ServiceBuilder {
	b.ports = append(b.ports, corev1.ServicePort{
		Name:       name,
		Port:       port,
		TargetPort: intstr.FromInt32(port),
		Protocol:   corev1.ProtocolTCP,
	})
	return b
}

// AddTCPPortWithTarget adds a TCP port to the Service with a different target port.
// Use this when the Service port (what clients connect to) differs from the container port
// (e.g., Service port 111 → container port 10111 for unprivileged portmapper).
func (b *ServiceBuilder) AddTCPPortWithTarget(name string, port, targetPort int32) *ServiceBuilder {
	b.ports = append(b.ports, corev1.ServicePort{
		Name:       name,
		Port:       port,
		TargetPort: intstr.FromInt32(targetPort),
		Protocol:   corev1.ProtocolTCP,
	})
	return b
}

// AddPort adds a pre-built ServicePort to the Service.
// Use this when the port already has the correct protocol set (e.g., UDP).
func (b *ServiceBuilder) AddPort(sp corev1.ServicePort) *ServiceBuilder {
	b.ports = append(b.ports, sp)
	return b
}

// Build constructs the Service object.
func (b *ServiceBuilder) Build() *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        b.name,
			Namespace:   b.namespace,
			Labels:      b.labels,
			Annotations: b.annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     b.serviceType,
			Selector: b.selector,
			Ports:    b.ports,
		},
	}

	if b.headless {
		svc.Spec.ClusterIP = corev1.ClusterIPNone
	}

	return svc
}
