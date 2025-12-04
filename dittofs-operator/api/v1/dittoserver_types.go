package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DittoServerSpec struct {
	Image    string      `json:"image,omitempty"`
	Replicas *int32      `json:"replicas,omitempty"`
	Storage  StorageSpec `json:"storage"`
	Config   DittoConfig `json:"config"`
	Service  ServiceSpec `json:"service,omitempty"`
}

type StorageSpec struct {
	MetadataSize     string  `json:"metadataSize"`
	ContentSize      string  `json:"contentSize"`
	StorageClassName *string `json:"storageClassName,omitempty"`
}

type DittoConfig struct {
	Shares   []ShareConfig   `json:"shares"`
	Backends []BackendConfig `json:"backends,omitempty"`
}

type ShareConfig struct {
	Name          string `json:"name"`
	ExportPath    string `json:"exportPath"` // "/export"
	MetadataStore string `json:"metadataStore"`
	ContentStore  string `json:"contentStore"`
}

type BackendConfig struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"` // "s3", "local", etc.
	Config map[string]string `json:"config,omitempty"`
}

type ServiceSpec struct {
	Type        string            `json:"type,omitempty"` // ClusterIP, LoadBalancer, etc.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ditto
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="NFS Endpoint",type=string,JSONPath=`.status.nfsEndpoint`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DittoServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DittoServerSpec   `json:"spec,omitempty"`
	Status DittoServerStatus `json:"status,omitempty"`
}

// DittoServerStatus defines the observed state of DittoServer
type DittoServerStatus struct {
	// Number of ready replicas
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// NFS endpoint that clients should use
	// Format: service-name.namespace.svc.cluster.local:2049
	NFSEndpoint string `json:"nfsEndpoint,omitempty"`

	// Phase of the server
	// +kubebuilder:validation:Enum=Pending;Running;Failed;Stopped
	Phase string `json:"phase,omitempty"`

	// Detailed status conditions
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// DittoServerList contains a list of DittoServer
type DittoServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DittoServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DittoServer{}, &DittoServerList{})
}
