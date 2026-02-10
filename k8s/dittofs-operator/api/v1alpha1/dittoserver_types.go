package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DittoServerSpec defines the desired state of DittoServer
// This is infrastructure-only configuration. Dynamic configuration (stores, shares,
// adapters, users) is managed via REST API at runtime.
type DittoServerSpec struct {
	// Image is the container image for DittoFS server
	// +kubebuilder:default="marmos91c/dittofs:latest"
	Image string `json:"image,omitempty"`

	// Replicas is the number of server replicas (0 or 1 only)
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Storage configures PVCs for internal server storage
	Storage StorageSpec `json:"storage"`

	// Database configures the control plane database
	// +optional
	Database *DatabaseConfig `json:"database,omitempty"`

	// Cache configures the WAL-backed cache
	// +optional
	Cache *InfraCacheConfig `json:"cache,omitempty"`

	// Metrics configures Prometheus metrics
	// +optional
	Metrics *MetricsConfig `json:"metrics,omitempty"`

	// ControlPlane configures the REST API server
	// +optional
	ControlPlane *ControlPlaneAPIConfig `json:"controlPlane,omitempty"`

	// Identity configures JWT authentication and admin user
	// +optional
	Identity *IdentityConfig `json:"identity,omitempty"`

	// AdapterDiscovery configures adapter discovery polling
	// +optional
	AdapterDiscovery *AdapterDiscoverySpec `json:"adapterDiscovery,omitempty"`

	// Service configures the Kubernetes Service
	Service ServiceSpec `json:"service,omitempty"`

	// NFSPort is the NFS server port
	// +kubebuilder:default=12049
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	NFSPort *int32 `json:"nfsPort,omitempty"`

	// SMB configures the SMB protocol adapter
	// +optional
	SMB *SMBAdapterSpec `json:"smb,omitempty"`

	// S3 configures S3-compatible payload store credentials
	// Credentials are injected as environment variables for the AWS SDK
	// +optional
	S3 *S3StoreConfig `json:"s3,omitempty"`

	// Percona configures auto-creation of PerconaPGCluster for PostgreSQL metadata store
	// When enabled, the operator creates a PerconaPGCluster owned by this DittoServer
	// +optional
	Percona *PerconaConfig `json:"percona,omitempty"`

	// Resources configures container resource requirements
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// SecurityContext for the container
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// PodSecurityContext for the pod
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

	// Size for cache PVC (mounted at /data/cache)
	// Required for WAL persistence - enables crash recovery
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
	// +kubebuilder:default="5Gi"
	// +kubebuilder:example="5Gi"
	CacheSize string `json:"cacheSize"`

	// StorageClass for the server's PVCs
	// If not specified, uses the cluster's default StorageClass
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// DatabaseConfig configures the control plane database
type DatabaseConfig struct {
	// Type is the database type: sqlite or postgres
	// +kubebuilder:validation:Enum=sqlite;postgres
	// +kubebuilder:default="sqlite"
	Type string `json:"type,omitempty"`

	// SQLite configuration (used when Type=sqlite)
	// +optional
	SQLite *SQLiteConfig `json:"sqlite,omitempty"`

	// PostgresSecretRef references a Secret containing the PostgreSQL connection string
	// The Secret must contain a key "connection-string" with the DSN
	// When configured, Postgres takes precedence over SQLite (regardless of Type field)
	// +optional
	PostgresSecretRef *corev1.SecretKeySelector `json:"postgresSecretRef,omitempty"`
}

// SQLiteConfig configures SQLite database
type SQLiteConfig struct {
	// Path is the database file path inside the container
	// +kubebuilder:default="/data/controlplane/controlplane.db"
	Path string `json:"path,omitempty"`
}

// InfraCacheConfig configures the WAL-backed cache for crash recovery
type InfraCacheConfig struct {
	// Path is the directory for cache WAL file
	// +kubebuilder:default="/data/cache"
	Path string `json:"path,omitempty"`

	// Size is the maximum cache size (e.g., "1GB", "512MB")
	// +kubebuilder:default="1GB"
	Size string `json:"size,omitempty"`
}

// MetricsConfig configures Prometheus metrics
type MetricsConfig struct {
	// Enabled controls whether metrics are exposed
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Port is the metrics HTTP port
	// +kubebuilder:default=9090
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// ControlPlaneAPIConfig configures the control plane REST API
type ControlPlaneAPIConfig struct {
	// Port is the API server port
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// SMBAdapterSpec defines SMB protocol configuration
type SMBAdapterSpec struct {
	// Enable SMB protocol
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// SMB port to listen on
	// +kubebuilder:default=12445
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

// IdentityConfig defines identity store and JWT authentication configuration
type IdentityConfig struct {
	// Type of identity store
	// +kubebuilder:default="memory"
	// +kubebuilder:validation:Enum=memory
	// +optional
	Type string `json:"type,omitempty"`

	// JWT configuration for REST API authentication
	// +optional
	JWT *JWTConfig `json:"jwt,omitempty"`

	// Admin user configuration
	// +optional
	Admin *AdminConfig `json:"admin,omitempty"`
}

// JWTConfig defines JWT authentication settings
type JWTConfig struct {
	// Reference to a Secret containing the JWT signing secret
	// The secret must contain a key with at least 32 characters
	// +kubebuilder:validation:Required
	SecretRef corev1.SecretKeySelector `json:"secretRef"`

	// Access token duration (e.g., "15m", "1h")
	// +kubebuilder:default="15m"
	// +optional
	AccessTokenDuration string `json:"accessTokenDuration,omitempty"`

	// Refresh token duration (e.g., "168h" for 7 days)
	// +kubebuilder:default="168h"
	// +optional
	RefreshTokenDuration string `json:"refreshTokenDuration,omitempty"`

	// Token issuer claim
	// +kubebuilder:default="dittofs"
	// +optional
	Issuer string `json:"issuer,omitempty"`
}

// AdminConfig defines the initial admin user configuration
type AdminConfig struct {
	// Admin username
	// +kubebuilder:default="admin"
	// +optional
	Username string `json:"username,omitempty"`

	// Reference to a Secret containing the admin password hash (bcrypt)
	// If not set, a random password will be generated and logged at startup
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`
}

// AdapterDiscoverySpec configures how the operator discovers protocol adapters.
type AdapterDiscoverySpec struct {
	// PollingInterval is how often the operator polls the adapter list API.
	// Supports Go duration strings (e.g., "30s", "1m", "5m").
	// +kubebuilder:default="30s"
	// +optional
	PollingInterval string `json:"pollingInterval,omitempty"`
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
	// ObservedGeneration is the generation last processed by the controller
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the desired number of replicas
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of pods with Ready condition
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of pods ready for at least minReadySeconds
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// NFSEndpoint that clients should use to mount
	// Format: service-name.namespace.svc.cluster.local:2049
	NFSEndpoint string `json:"nfsEndpoint,omitempty"`

	// Phase of the DittoServer (Pending, Running, Failed, Stopped, Deleting)
	// +kubebuilder:validation:Enum=Pending;Running;Failed;Stopped;Deleting
	Phase string `json:"phase,omitempty"`

	// ConfigHash is the hash of current configuration (for debugging)
	ConfigHash string `json:"configHash,omitempty"`

	// PerconaClusterName is the name of the owned PerconaPGCluster (when enabled)
	// +optional
	PerconaClusterName string `json:"perconaClusterName,omitempty"`

	// Conditions represent the latest available observations
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ditto;dittofs
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
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

// S3CredentialsSecretRef references a Secret containing S3-compatible credentials
type S3CredentialsSecretRef struct {
	// Name of the Secret in the same namespace
	// +kubebuilder:validation:Required
	SecretName string `json:"secretName"`

	// Key for access key ID (default: accessKeyId)
	// +kubebuilder:default="accessKeyId"
	AccessKeyIDKey string `json:"accessKeyIdKey,omitempty"`

	// Key for secret access key (default: secretAccessKey)
	// +kubebuilder:default="secretAccessKey"
	SecretAccessKeyKey string `json:"secretAccessKeyKey,omitempty"`

	// Key for S3 endpoint URL (default: endpoint)
	// For Cubbit DS3: https://s3.cubbit.eu
	// +kubebuilder:default="endpoint"
	EndpointKey string `json:"endpointKey,omitempty"`
}

// S3StoreConfig configures S3-compatible payload store credentials
// Note: Actual store creation is done via REST API; this enables
// the operator to inject S3 credentials as environment variables.
type S3StoreConfig struct {
	// CredentialsSecretRef references a Secret with S3 credentials
	// +optional
	CredentialsSecretRef *S3CredentialsSecretRef `json:"credentialsSecretRef,omitempty"`

	// Region for S3 bucket (e.g., "eu-west-1" for Cubbit)
	// +kubebuilder:default="eu-west-1"
	Region string `json:"region,omitempty"`

	// Bucket name (informational; actual config via REST API)
	// +optional
	Bucket string `json:"bucket,omitempty"`
}

// PerconaConfig configures auto-creation of PerconaPGCluster for PostgreSQL metadata store
type PerconaConfig struct {
	// Enabled triggers auto-creation of PerconaPGCluster
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// DeleteWithServer controls whether PerconaPGCluster is deleted when DittoServer is deleted.
	// If true, the PostgreSQL cluster and its data are deleted with the DittoServer.
	// If false (default), the PerconaPGCluster is orphaned and preserved.
	// WARNING: Setting to true will delete all PostgreSQL data when DittoServer is deleted!
	// +kubebuilder:default=false
	// +optional
	DeleteWithServer bool `json:"deleteWithServer,omitempty"`

	// Replicas for PostgreSQL instances
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// StorageSize for PostgreSQL data volume
	// +kubebuilder:default="10Gi"
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	// StorageClassName for PostgreSQL PVCs (may differ from DittoFS storage)
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// DatabaseName for DittoFS control plane
	// +kubebuilder:default="dittofs"
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// Backup configures pgBackRest S3 backups (optional)
	// +optional
	Backup *PerconaBackupConfig `json:"backup,omitempty"`
}

// PerconaBackupConfig configures pgBackRest S3 backups for PostgreSQL
type PerconaBackupConfig struct {
	// Enabled activates backup configuration
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// CredentialsSecretRef references Secret with S3 credentials for pgBackRest
	// Secret must contain keys matching pgBackRest s3.conf format
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`

	// Bucket name for backups
	// +optional
	Bucket string `json:"bucket,omitempty"`

	// Endpoint for S3-compatible storage (e.g., https://s3.cubbit.eu)
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Region for S3 bucket
	// +kubebuilder:default="eu-west-1"
	// +optional
	Region string `json:"region,omitempty"`

	// FullSchedule cron expression for full backups
	// +kubebuilder:default="0 2 * * *"
	// +optional
	FullSchedule string `json:"fullSchedule,omitempty"`

	// IncrSchedule cron expression for incremental backups
	// +kubebuilder:default="0 * * * *"
	// +optional
	IncrSchedule string `json:"incrSchedule,omitempty"`

	// RetentionDays for backup retention
	// +kubebuilder:default=7
	// +kubebuilder:validation:Minimum=1
	// +optional
	RetentionDays *int32 `json:"retentionDays,omitempty"`
}

func init() {
	SchemeBuilder.Register(&DittoServer{}, &DittoServerList{})
}
