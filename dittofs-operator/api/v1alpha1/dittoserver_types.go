package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/metaSvc/v1"
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

	// NFS port to listen on
	// +kubebuilder:default=2049
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	NFSPort *int32 `json:"nfsPort,omitempty"`

	// SMB adapter configuration
	// +optional
	SMB *SMBAdapterSpec `json:"smb,omitempty"`

	// User management configuration
	// +optional
	Users *UserManagementSpec `json:"users,omitempty"`

	// Resource requirements for the DittoFS container (CPU, memory limits/requests)
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Security context for the DittoFS container
	// Use this to configure capabilities (e.g., CAP_SYS_ADMIN for privileged ports)
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// Pod security context for the DittoFS pod
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
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
	// +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
	// +kubebuilder:example="50Gi"
	ContentSize string `json:"contentSize,omitempty"`

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

	// Cache configuration for read/write buffering
	// Caches are optional and referenced by name in ShareConfig
	// +optional
	Caches []CacheConfig `json:"caches,omitempty"`
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

	// Reference to a cache in the Caches list (by name)
	// Cache is optional - if not specified, no caching is used
	// +optional
	Cache string `json:"cache,omitempty"`

	// ReadOnly makes the share read-only if true
	// +kubebuilder:default=false
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`

	// AllowedClients lists IP addresses or CIDR ranges allowed to access this share
	// Empty list means all clients are allowed
	// +optional
	AllowedClients []string `json:"allowedClients,omitempty"`

	// DeniedClients lists IP addresses or CIDR ranges explicitly denied access
	// Takes precedence over AllowedClients
	// +optional
	DeniedClients []string `json:"deniedClients,omitempty"`

	// RequireAuth requires authentication if true
	// +kubebuilder:default=false
	// +optional
	RequireAuth bool `json:"requireAuth,omitempty"`

	// AllowedAuthMethods lists allowed authentication methods
	// Valid values: anonymous, unix
	// +kubebuilder:default={"anonymous","unix"}
	// +optional
	AllowedAuthMethods []string `json:"allowedAuthMethods,omitempty"`

	// AllowGuest allows guest/anonymous access to this share
	// +kubebuilder:default=false
	// +optional
	AllowGuest *bool `json:"allowGuest,omitempty"`

	// DefaultPermission sets the default permission level for users without explicit permissions
	// Valid values: none, read, read-write, admin
	// +kubebuilder:default="read"
	// +kubebuilder:validation:Enum=none;read;read-write;admin
	// +optional
	DefaultPermission string `json:"defaultPermission,omitempty"`

	// IdentityMapping configures user/group mapping for this share
	// +optional
	IdentityMapping *IdentityMappingConfig `json:"identityMapping,omitempty"`

	// RootDirectoryAttributes sets the attributes for the root directory of the share
	// +optional
	RootDirectoryAttributes *DirectoryAttributesConfig `json:"rootDirectoryAttributes,omitempty"`

	// DumpRestricted restricts dump operations if true
	// +kubebuilder:default=false
	// +optional
	DumpRestricted bool `json:"dumpRestricted,omitempty"`

	// DumpAllowedClients lists clients allowed to perform dump operations
	// Only relevant if DumpRestricted is true
	// +optional
	DumpAllowedClients []string `json:"dumpAllowedClients,omitempty"`
}

// IdentityMappingConfig defines user/group mapping configuration for NFS shares
type IdentityMappingConfig struct {
	// MapAllToAnonymous maps all users to anonymous if true
	// +kubebuilder:default=false
	// +optional
	MapAllToAnonymous bool `json:"mapAllToAnonymous,omitempty"`

	// MapPrivilegedToAnonymous maps root/privileged users to anonymous if true
	// +kubebuilder:default=false
	// +optional
	MapPrivilegedToAnonymous bool `json:"mapPrivilegedToAnonymous,omitempty"`

	// AnonymousUID is the UID used for anonymous users
	// +kubebuilder:default=65534
	// +optional
	AnonymousUID int32 `json:"anonymousUID,omitempty"`

	// AnonymousGID is the GID used for anonymous users
	// +kubebuilder:default=65534
	// +optional
	AnonymousGID int32 `json:"anonymousGID,omitempty"`
}

// DirectoryAttributesConfig defines directory ownership and permissions
type DirectoryAttributesConfig struct {
	// Mode is the file mode/permissions (e.g., 0755)
	// +kubebuilder:default=493
	// +optional
	Mode int32 `json:"mode,omitempty"`

	// UID is the user ID of the directory owner
	// +kubebuilder:default=0
	// +optional
	UID int32 `json:"uid,omitempty"`

	// GID is the group ID of the directory owner
	// +kubebuilder:default=0
	// +optional
	GID int32 `json:"gid,omitempty"`
}

// CacheConfig defines a cache instance for read/write buffering
// Caches are referenced by name in ShareConfig
type CacheConfig struct {
	// Unique name for this cache
	// Used as a reference in ShareConfig.Cache field
	// +kubebuilder:validation:Required
	// +kubebuilder:example="fast-cache"
	Name string `json:"name"`

	// Type of cache implementation
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=memory
	// +kubebuilder:default="memory"
	Type string `json:"type"`

	// Memory cache configuration (only used when Type=memory)
	// +optional
	Memory *MemoryCacheConfig `json:"memory,omitempty"`

	// Prefetch configuration for read optimization
	// +optional
	Prefetch *PrefetchConfig `json:"prefetch,omitempty"`

	// Flusher configuration for write buffering and background flush
	// +optional
	Flusher *FlusherConfig `json:"flusher,omitempty"`
}

// MemoryCacheConfig defines configuration for in-memory cache
type MemoryCacheConfig struct {
	// MaxSize is the maximum cache size in bytes
	// Use suffixes like "1Gi", "512Mi", etc.
	// Empty or "0" means unlimited
	// +kubebuilder:example="1Gi"
	// +optional
	MaxSize string `json:"maxSize,omitempty"`
}

// PrefetchConfig defines read prefetch optimization settings
type PrefetchConfig struct {
	// Enabled controls whether prefetch is enabled
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// MaxFileSize is the maximum file size to prefetch
	// Files larger than this are not prefetched to avoid cache thrashing
	// Use suffixes like "100Mi", "1Gi", etc.
	// +kubebuilder:example="100Mi"
	// +kubebuilder:default="100Mi"
	// +optional
	MaxFileSize string `json:"maxFileSize,omitempty"`

	// ChunkSize is the size of each chunk read during prefetch
	// +kubebuilder:example="512Ki"
	// +kubebuilder:default="512Ki"
	// +optional
	ChunkSize string `json:"chunkSize,omitempty"`
}

// FlusherConfig defines write buffering and background flush settings
type FlusherConfig struct {
	// SweepInterval is how often to check for idle files
	// +kubebuilder:example="10s"
	// +kubebuilder:default="10s"
	// +optional
	Interval string `json:"interval,omitempty"`

	// FlushTimeout is how long a file must be idle before flushing
	// This is the key NFS async write timeout
	// +kubebuilder:example="30s"
	// +kubebuilder:default="30s"
	// +optional
	FlushTimeout string `json:"flushTimeout,omitempty"`

	// Workers is the number of parallel flush workers
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	// +optional
	Workers *int32 `json:"workers,omitempty"`
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

	// Secret references for sensitive configuration values
	// Keys should match the expected config keys (e.g., "access_key_id", "secret_access_key")
	// +optional
	SecretRefs map[string]corev1.SecretKeySelector `json:"secretRefs,omitempty"`
}

// SMBAdapterSpec defines SMB protocol configuration
type SMBAdapterSpec struct {
	// Enable SMB protocol
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// SMB port to listen on
	// +kubebuilder:default=445
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// Maximum number of concurrent SMB connections (0 = unlimited)
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxConnections *int32 `json:"maxConnections,omitempty"`

	// Maximum number of concurrent requests per SMB connection
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxRequestsPerConnection *int32 `json:"maxRequestsPerConnection,omitempty"`

	// Timeout configurations for SMB operations
	// +optional
	Timeouts *SMBTimeoutsSpec `json:"timeouts,omitempty"`

	// SMB credit management configuration
	// +optional
	Credits *SMBCreditsSpec `json:"credits,omitempty"`

	// Metrics logging interval (e.g., "5m", "10m")
	// +kubebuilder:default="5m"
	// +optional
	MetricsLogInterval string `json:"metricsLogInterval,omitempty"`
}

// SMBTimeoutsSpec defines timeout configurations for SMB operations
type SMBTimeoutsSpec struct {
	// Maximum time to read request
	// +kubebuilder:default="5m"
	// +optional
	Read string `json:"read,omitempty"`

	// Maximum time to write response
	// +kubebuilder:default="30s"
	// +optional
	Write string `json:"write,omitempty"`

	// Maximum idle time between requests
	// +kubebuilder:default="5m"
	// +optional
	Idle string `json:"idle,omitempty"`

	// Graceful shutdown timeout
	// +kubebuilder:default="30s"
	// +optional
	Shutdown string `json:"shutdown,omitempty"`
}

// SMBCreditsSpec defines SMB credit management configuration
type SMBCreditsSpec struct {
	// Credit grant strategy: fixed, echo, or adaptive
	// +kubebuilder:default="adaptive"
	// +kubebuilder:validation:Enum=fixed;echo;adaptive
	// +optional
	Strategy string `json:"strategy,omitempty"`

	// Minimum credits per response
	// +kubebuilder:default=16
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinGrant *int32 `json:"minGrant,omitempty"`

	// Maximum credits per response
	// +kubebuilder:default=8192
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxGrant *int32 `json:"maxGrant,omitempty"`

	// Credits for initial requests (NEGOTIATE)
	// +kubebuilder:default=256
	// +kubebuilder:validation:Minimum=1
	// +optional
	InitialGrant *int32 `json:"initialGrant,omitempty"`

	// Maximum outstanding credits per session
	// +kubebuilder:default=65535
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxSessionCredits *int32 `json:"maxSessionCredits,omitempty"`

	// Server load threshold for throttling (adaptive strategy only)
	// +kubebuilder:default=1000
	// +kubebuilder:validation:Minimum=1
	// +optional
	LoadThresholdHigh *int32 `json:"loadThresholdHigh,omitempty"`

	// Server load threshold for boosting (adaptive strategy only)
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +optional
	LoadThresholdLow *int32 `json:"loadThresholdLow,omitempty"`

	// Outstanding requests threshold for client throttling (adaptive strategy only)
	// +kubebuilder:default=256
	// +kubebuilder:validation:Minimum=1
	// +optional
	AggressiveClientThreshold *int32 `json:"aggressiveClientThreshold,omitempty"`
}

// UserManagementSpec defines user and group management configuration
type UserManagementSpec struct {
	// List of users
	// +optional
	Users []UserSpec `json:"users,omitempty"`

	// List of groups
	// +optional
	Groups []GroupSpec `json:"groups,omitempty"`

	// Guest configuration for anonymous access
	// +optional
	Guest *GuestSpec `json:"guest,omitempty"`
}

// UserSpec defines a user with credentials and permissions
type UserSpec struct {
	// Username for authentication
	// +kubebuilder:validation:Required
	Username string `json:"username"`

	// Password hash (bcrypt) - DEPRECATED: Use passwordSecretRef instead
	// Generate with: htpasswd -bnBC 10 "" password | tr -d ':\n'
	// +optional
	PasswordHash string `json:"passwordHash,omitempty"`

	// Reference to a Secret key containing the bcrypt password hash
	// The Secret name and key are specified via the SecretKeySelector
	// For example, you may store the hash under a key such as "passwordHash"
	// This is the preferred way to store user passwords
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// Whether the user is enabled
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Unix UID for NFS identity mapping
	// +kubebuilder:validation:Required
	UID uint32 `json:"uid"`

	// Primary Unix GID
	// +kubebuilder:validation:Required
	GID uint32 `json:"gid"`

	// Group membership (by group name)
	// +optional
	Groups []string `json:"groups,omitempty"`

	// Per-share permissions (overrides group permissions)
	// Map of share path to permission level (none, read, read-write, admin)
	// +optional
	SharePermissions map[string]string `json:"sharePermissions,omitempty"`
}

// GroupSpec defines a group with share-level permissions
type GroupSpec struct {
	// Unique group name
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Unix GID
	// +kubebuilder:validation:Required
	GID uint32 `json:"gid"`

	// Per-share permissions for all group members
	// Map of share path to permission level (none, read, read-write, admin)
	// +optional
	SharePermissions map[string]string `json:"sharePermissions,omitempty"`
}

// GuestSpec defines guest/anonymous access configuration
type GuestSpec struct {
	// Enable guest/anonymous access
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Unix UID for guest users
	// +kubebuilder:default=65534
	// +optional
	UID uint32 `json:"uid,omitempty"`

	// Unix GID for guest users
	// +kubebuilder:default=65534
	// +optional
	GID uint32 `json:"gid,omitempty"`

	// Per-share permissions for guests
	// Map of share path to permission level (none, read, read-write, admin)
	// +optional
	SharePermissions map[string]string `json:"sharePermissions,omitempty"`
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
	metav1.ListMeta `json:"metadata"`
	Items           []DittoServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DittoServer{}, &DittoServerList{})
}
