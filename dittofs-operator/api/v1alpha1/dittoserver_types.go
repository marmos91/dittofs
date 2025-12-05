package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DittoServerSpec defines the desired state of DittoServer
type DittoServerSpec struct {
	// Container image for DittoFS server
	// +kubebuilder:default="dittofs/dittofs:latest"
	Image string `json:"image,omitempty"`

	// Number of server replicas
	// Only 0 or 1 is supported. Multiple replicas would cause BadgerDB corruption.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Storage configuration for DittoFS server's internal volumes
	// These PVCs are mounted inside the server pod at /data/metadata and /data/content
	// This is separate from the NFS storage exposed to clients
	Storage StorageSpec `json:"storage"`

	// DittoFS configuration (shares, backends, etc.)
	// This maps directly to the config.yaml format used by DittoFS
	Config DittoConfig `json:"config"`

	// Service configuration for the NFS server endpoint
	Service ServiceSpec `json:"service,omitempty"`
}

// StorageSpec defines storage volumes for the DittoFS server pod's internal use
// The operator creates PVCs based on these specs and mounts them in the server pod
type StorageSpec struct {
	// Size for metadata store PVC (mounted at /data/metadata)
	// Used by BadgerDB or other metadata backends
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
	// +kubebuilder:example="10Gi"
	MetadataSize string `json:"metadataSize"`

	// Size for content store PVC (mounted at /data/content)
	// Used for local filesystem content backend (not needed if using pure S3)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
	// +kubebuilder:example="50Gi"
	ContentSize string `json:"contentSize"`

	// StorageClass for the server's PVCs
	// If not specified, uses the cluster's default StorageClass
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// DittoConfig contains DittoFS server configuration
// This structure mirrors the DittoFS config.yaml format
type DittoConfig struct {
	// NFS exports/shares configuration
	// Each share defines an NFS export path and its backing stores
	// +kubebuilder:validation:MinItems=1
	Shares []ShareConfig `json:"shares"`

	// Backend storage definitions (metadata and content stores)
	// These are referenced by name in ShareConfig
	Backends []BackendConfig `json:"backends,omitempty"`
}

// ShareConfig defines an NFS export/share
type ShareConfig struct {
	// Unique name for this share
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// NFS export path (e.g., "/export", "/data")
	// +kubebuilder:validation:Required
	// +kubebuilder:default="/export"
	ExportPath string `json:"exportPath"`

	// Reference to a backend in the Backends list (by name)
	// This backend will store filesystem metadata
	// +kubebuilder:validation:Required
	MetadataStore string `json:"metadataStore"`

	// Reference to a backend in the Backends list (by name)
	// This backend will store file content
	// +kubebuilder:validation:Required
	ContentStore string `json:"contentStore"`
}

// BackendConfig defines a storage backend (metadata or content store)
// Backends are referenced by name in ShareConfig
type BackendConfig struct {
	// Unique name for this backend
	// Used as a reference in MetadataStore/ContentStore fields
	// +kubebuilder:validation:Required
	// +kubebuilder:example="s3-production"
	Name string `json:"name"`

	// Type of backend storage
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=s3;local;badger
	Type string `json:"type"`

	// Backend-specific configuration (e.g., S3 bucket, region, credentials)
	// The structure depends on the Type field
	// +kubebuilder:example={"bucket":"my-bucket","region":"us-east-1"}
	Config map[string]string `json:"config,omitempty"`
}

// ServiceSpec defines the Kubernetes Service for the NFS server
type ServiceSpec struct {
	// Service type
	// +kubebuilder:default="ClusterIP"
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	Type string `json:"type,omitempty"`

	// Service annotations (e.g., for cloud load balancer configuration)
	Annotations map[string]string `json:"annotations,omitempty"`
}

// DittoServerStatus defines the observed state of DittoServer
type DittoServerStatus struct {
	// Number of ready replicas
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// NFS endpoint that clients should use to mount
	// Format: service-name.namespace.svc.cluster.local:2049
	NFSEndpoint string `json:"nfsEndpoint,omitempty"`

	// Current phase of the DittoServer
	// +kubebuilder:validation:Enum=Pending;Running;Failed;Stopped
	Phase string `json:"phase,omitempty"`

	// Detailed status conditions
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ditto
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="NFS Endpoint",type=string,JSONPath=`.status.nfsEndpoint`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DittoServer is the Schema for the dittoservers API
type DittoServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DittoServerSpec   `json:"spec,omitempty"`
	Status DittoServerStatus `json:"status,omitempty"`
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
