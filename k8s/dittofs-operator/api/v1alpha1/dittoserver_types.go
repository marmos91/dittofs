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

	// ControlPlane configures the REST API server
	// +optional
	ControlPlane *ControlPlaneAPIConfig `json:"controlPlane,omitempty"`

	// Identity configures JWT authentication and admin user
	// +optional
	Identity *IdentityConfig `json:"identity,omitempty"`

	// AdapterDiscovery configures adapter discovery polling
	// +optional
	AdapterDiscovery *AdapterDiscoverySpec `json:"adapterDiscovery,omitempty"`

	// AdapterServices configures dynamically created per-adapter Services.
	// +optional
	AdapterServices *AdapterServiceConfig `json:"adapterServices,omitempty"`

	// Service configures the Kubernetes Service
	Service ServiceSpec `json:"service,omitempty"`

	// Metrics configures the Prometheus /metrics surface and its integration
	// with an in-cluster Prometheus (metrics Service, scrape annotations,
	// optional ServiceMonitor). Opt-in; disabled by default.
	// +optional
	Metrics *MetricsSpec `json:"metrics,omitempty"`

	// Percona configures auto-creation of PerconaPGCluster for PostgreSQL metadata store
	// When enabled, the operator creates a PerconaPGCluster owned by this DittoServer
	// +optional
	Percona *PerconaConfig `json:"percona,omitempty"`

	// SnapshotPolicies declares per-share scheduled snapshot policies. The
	// operator pushes each entry to the DittoFS API once authenticated; shares
	// not yet created are skipped and retried on the next reconcile. The
	// operator only upserts the declared policies — it does not delete policies
	// removed from this list, so manually-created policies are preserved.
	// +optional
	SnapshotPolicies []ShareSnapshotPolicySpec `json:"snapshotPolicies,omitempty"`

	// Resources configures container resource requirements
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// TerminationGracePeriodSeconds is the pod's grace period for the dfs server
	// to complete its graceful shutdown on rollout, drain, or scale-down.
	//
	// The dfs server runs shutdown stages serially, each bounded by its configured
	// shutdown_timeout (30s). Combined with the PreStop hook delay, the Kubernetes
	// default of 30s is too short and causes a SIGKILL mid-flush, which can lose
	// metadata. When unset, the operator derives a safe value from the configured
	// shutdown_timeout (preStop + 3*shutdownTimeout + buffer). Override only if you
	// understand your shutdown budget; it must comfortably exceed the shutdown_timeout.
	// +kubebuilder:validation:Minimum=36
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

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
	// Size for the metadata store PVC (mounted at /data/store/metadata)
	// Used by BadgerDB or other metadata backends
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
	// +kubebuilder:example="10Gi"
	MetadataSize string `json:"metadataSize"`

	// Size for the block/content store PVC (mounted at /data/store/block).
	// Holds local CAS chunks and the block-store append-log (the durable WAL
	// replayed on crash recovery). Not needed for pure-S3 shares.
	// +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
	// +kubebuilder:example="50Gi"
	ContentSize string `json:"contentSize,omitempty"`

	// Size for the control-plane PVC (mounted at /data/controlplane), which
	// holds the control-plane SQLite database (metadata-store registry + share
	// definitions). Small by nature; defaults to 1Gi.
	// +kubebuilder:validation:Pattern=`^[0-9]+(Gi|Mi|Ti)$`
	// +kubebuilder:default="1Gi"
	// +kubebuilder:example="1Gi"
	// +optional
	ControlPlaneSize string `json:"controlPlaneSize,omitempty"`

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
	// Path is the database file path inside the container. It MUST live under a
	// persistent volume (the operator mounts the control-plane PVC at
	// /data/controlplane) — a path on the ephemeral container overlay is wiped on
	// every pod restart, silently dropping all metadata-store and share definitions.
	// +kubebuilder:default="/data/controlplane/controlplane.db"
	Path string `json:"path,omitempty"`
}

// ControlPlaneAPIConfig configures the control plane REST API
type ControlPlaneAPIConfig struct {
	// Port is the API server port
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// TLS controls the URL scheme the operator uses to reach the control plane
	// API. When true the operator connects over https:// so the admin and
	// operator service-account credentials it sends are not transmitted in
	// cleartext over the pod network. This requires the dfs control plane API to
	// be served over TLS — either natively (see CertSecretName, which makes the
	// pod serve HTTPS end-to-end) or terminated by an in-cluster mesh/sidecar
	// that exposes the API service on https. Defaults to false (http://) for
	// backward compatibility; in-cluster credentialed deployments should enable
	// it once TLS termination is in place.
	// +kubebuilder:default=false
	// +optional
	TLS bool `json:"tls,omitempty"`

	// CertSecretName names a Kubernetes Secret containing the server
	// certificate and private key the dfs pod uses to serve native TLS
	// (HTTPS) on the control plane API. When set, the operator mounts the
	// Secret read-only into the dfs container and renders
	// controlplane.tls.cert_file / key_file so the API is served over TLS
	// end-to-end inside the cluster, rather than relying only on edge
	// termination.
	//
	// The Secret must use the standard kubernetes.io/tls keys "tls.crt" and
	// "tls.key" (e.g. a cert-manager-issued Certificate Secret). Setting this
	// implies TLS=true (https scheme) for the operator's own API calls.
	// +optional
	CertSecretName string `json:"certSecretName,omitempty"`

	// ClientCASecretName optionally names a Secret containing a CA bundle used
	// to require and verify client certificates (mutual TLS) on the control
	// plane API. The CA bundle must be stored under the key "ca.crt". Only
	// honored when CertSecretName is also set (mTLS needs a server cert).
	// +optional
	ClientCASecretName string `json:"clientCASecretName,omitempty"`
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

// AdapterServiceConfig configures dynamically created per-adapter Services.
type AdapterServiceConfig struct {
	// Type of Service to create for each adapter (LoadBalancer, NodePort, ClusterIP).
	// +kubebuilder:default="LoadBalancer"
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	Type string `json:"type,omitempty"`

	// Annotations to apply to adapter Services (e.g., cloud LB configuration).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
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

// MetricsSpec configures the DittoFS Prometheus /metrics surface and its
// integration with an in-cluster Prometheus. The endpoint is opt-in and the
// integration glue (metrics Service, scrape annotations, ServiceMonitor) is
// emitted ONLY when explicitly enabled here.
//
// Ownership model: DittoFS provides the scrape target and the optional
// integration objects only. It never runs, bundles, or depends on a
// Prometheus/Thanos instance — that is the cluster's responsibility (typically
// the prometheus-operator / kube-prometheus-stack). The ServiceMonitor is
// additionally gated by a CRD-discovery check: if monitoring.coreos.com CRDs
// are absent, it is skipped (logged at Info) and never fails the reconcile.
type MetricsSpec struct {
	// Enabled turns on the DittoFS metrics endpoint and renders the metrics
	// container port plus a metrics Service carrying prometheus.io/scrape
	// annotations for annotation-based discovery. Default: false (opt-in).
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Port is the TCP port the metrics endpoint binds to inside the container
	// and the port the metrics Service exposes. Default: 9090.
	// +kubebuilder:default=9090
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Path is the HTTP path the metrics are served on. Default: /metrics.
	// Must start with "/" (matches the server-side config.Validate check).
	// +kubebuilder:default="/metrics"
	// +kubebuilder:validation:Pattern=`^/.*`
	// +optional
	Path string `json:"path,omitempty"`

	// BearerTokenSecret optionally references a Secret holding a bearer token
	// the endpoint requires for scraping (server auth mode "token"). The token
	// file is mounted into the dfs container and the matching ServiceMonitor (if
	// emitted) is configured to present the same token. When unset the endpoint
	// is unauthenticated and relies on the metrics Port + NetworkPolicy for
	// isolation.
	// +optional
	BearerTokenSecret *corev1.SecretKeySelector `json:"bearerTokenSecret,omitempty"`

	// ServiceMonitor configures emission of a monitoring.coreos.com/v1
	// ServiceMonitor selecting the metrics Service, for clusters running the
	// prometheus-operator.
	// +optional
	ServiceMonitor *ServiceMonitorSpec `json:"serviceMonitor,omitempty"`
}

// ServiceMonitorSpec configures the optional prometheus-operator ServiceMonitor.
type ServiceMonitorSpec struct {
	// Enabled requests creation of a ServiceMonitor for the metrics Service.
	// It is honored only when the monitoring.coreos.com/v1 CRD is present in the
	// cluster; otherwise it is skipped (logged) and the reconcile does not fail.
	// Default: false.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Interval is the Prometheus scrape interval (e.g. "30s"). When empty the
	// Prometheus default applies.
	// +optional
	Interval string `json:"interval,omitempty"`

	// Labels are added to the ServiceMonitor's metadata.labels so it can be
	// matched by a Prometheus serviceMonitorSelector (e.g. {release: kube-prometheus-stack}).
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
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

// ShareSnapshotPolicySpec declares a scheduled snapshot policy for one share.
// It maps to the DittoFS snapshot-policy REST surface
// (PUT /api/v1/shares/{share}/snapshot-policy).
type ShareSnapshotPolicySpec struct {
	// Share is the share name the policy applies to (e.g. "/archive").
	Share string `json:"share"`

	// Interval is the snapshot cadence: a Go duration ("24h", "6h") or an
	// @-shorthand (@hourly, @daily, @weekly).
	Interval string `json:"interval"`

	// KeepLast keeps only the newest N scheduler-created snapshots (0 = no
	// count bound).
	// +kubebuilder:validation:Minimum=0
	// +optional
	KeepLast *int32 `json:"keepLast,omitempty"`

	// TTL prunes scheduler-created snapshots older than this Go duration
	// (empty = no age bound).
	// +optional
	TTL string `json:"ttl,omitempty"`

	// Enabled toggles automatic snapshotting. Defaults to true when omitted.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// NamePrefix labels scheduler-created snapshots (default "scheduled").
	// +optional
	NamePrefix string `json:"namePrefix,omitempty"`
}

func init() {
	SchemeBuilder.Register(&DittoServer{}, &DittoServerList{})
}
