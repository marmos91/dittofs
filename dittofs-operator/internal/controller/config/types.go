package config

// DittoFSConfig represents the complete DittoFS configuration structure
type DittoFSConfig struct {
	Logging  LoggingConfig   `yaml:"logging"`
	Server   ServerConfig    `yaml:"server"`
	Identity *IdentityConfig `yaml:"identity,omitempty"`
	Metadata MetadataConfig  `yaml:"metadata"`
	Content  ContentConfig   `yaml:"content"`
	Cache    CacheConfig     `yaml:"cache,omitempty"`
	Shares   []Share         `yaml:"shares"`
	Users    []User          `yaml:"users,omitempty"`
	Groups   []Group         `yaml:"groups,omitempty"`
	Guest    *Guest          `yaml:"guest,omitempty"`
	Adapters AdaptersConfig  `yaml:"adapters"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	Output string `yaml:"output"`
}

type ServerConfig struct {
	ShutdownTimeout string `yaml:"shutdown_timeout"`
}

// IdentityConfig defines identity store and JWT authentication configuration
type IdentityConfig struct {
	Type  string     `yaml:"type"`
	JWT   *JWTConfig `yaml:"jwt,omitempty"`
	Admin *Admin     `yaml:"admin,omitempty"`
}

// JWTConfig defines JWT authentication settings
type JWTConfig struct {
	Secret               string `yaml:"secret"`
	Issuer               string `yaml:"issuer,omitempty"`
	AccessTokenDuration  string `yaml:"access_token_duration,omitempty"`
	RefreshTokenDuration string `yaml:"refresh_token_duration,omitempty"`
}

// Admin defines the initial admin user configuration
type Admin struct {
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

type MetadataConfig struct {
	Global MetadataGlobal           `yaml:"global"`
	Stores map[string]MetadataStore `yaml:"stores"`
}

type MetadataGlobal struct {
	FilesystemCapabilities FilesystemCapabilities `yaml:"filesystem_capabilities"`
}

type FilesystemCapabilities struct {
	MaxReadSize        int64 `yaml:"max_read_size"`
	PreferredReadSize  int64 `yaml:"preferred_read_size"`
	MaxWriteSize       int64 `yaml:"max_write_size"`
	PreferredWriteSize int64 `yaml:"preferred_write_size"`
	MaxFileSize        int64 `yaml:"max_file_size"`
	MaxFilenameLen     int   `yaml:"max_filename_len"`
	MaxPathLen         int   `yaml:"max_path_len"`
	MaxHardLinkCount   int   `yaml:"max_hard_link_count"`
	SupportsHardLinks  bool  `yaml:"supports_hard_links"`
	SupportsSymlinks   bool  `yaml:"supports_symlinks"`
	CaseSensitive      bool  `yaml:"case_sensitive"`
	CasePreserving     bool  `yaml:"case_preserving"`
}

type MetadataStore struct {
	Type   string         `yaml:"type"`
	Badger *BadgerConfig  `yaml:"badger,omitempty"`
	Config map[string]any `yaml:",inline,omitempty"`
}

type BadgerConfig struct {
	DBPath string `yaml:"db_path"`
}

type ContentConfig struct {
	Global map[string]any          `yaml:"global"`
	Stores map[string]ContentStore `yaml:"stores"`
}

type ContentStore struct {
	Type       string            `yaml:"type"`
	Filesystem *FilesystemConfig `yaml:"filesystem,omitempty"`
	S3         *S3Config         `yaml:"s3,omitempty"`
	Config     map[string]any    `yaml:",inline,omitempty"`
}

type FilesystemConfig struct {
	Path string `yaml:"path"`
}

type S3Config struct {
	Bucket          string `yaml:"bucket,omitempty"`
	Region          string `yaml:"region,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty"`
	AccessKeyID     string `yaml:"access_key_id,omitempty"`
	SecretAccessKey string `yaml:"secret_access_key,omitempty"`
	ForcePathStyle  bool   `yaml:"force_path_style,omitempty"`
}

type CacheConfig struct {
	Stores map[string]CacheStore `yaml:"stores,omitempty"`
}

type CacheStore struct {
	Type     string         `yaml:"type"`
	Memory   map[string]any `yaml:"memory,omitempty"`
	Prefetch *Prefetch      `yaml:"prefetch,omitempty"`
	Flusher  *Flusher       `yaml:"flusher,omitempty"`
}

type Prefetch struct {
	Enabled     *bool `yaml:"enabled,omitempty"`
	MaxFileSize any   `yaml:"max_file_size,omitempty"`
	ChunkSize   any   `yaml:"chunk_size,omitempty"`
}

type Flusher struct {
	SweepInterval string `yaml:"sweep_interval,omitempty"`
	FlushTimeout  string `yaml:"flush_timeout,omitempty"`
	FlushPoolSize *int32 `yaml:"flush_pool_size,omitempty"`
}

type Share struct {
	Name                    string              `yaml:"name"`
	MetadataStore           string              `yaml:"metadata_store"`
	ContentStore            string              `yaml:"content_store"`
	Cache                   string              `yaml:"cache,omitempty"`
	ReadOnly                bool                `yaml:"read_only"`
	DefaultPermission       string              `yaml:"default_permission,omitempty"`
	AllowedClients          []string            `yaml:"allowed_clients"`
	DeniedClients           []string            `yaml:"denied_clients"`
	RequireAuth             bool                `yaml:"require_auth"`
	AllowedAuthMethods      []string            `yaml:"allowed_auth_methods"`
	IdentityMapping         IdentityMapping     `yaml:"identity_mapping"`
	RootDirectoryAttributes DirectoryAttributes `yaml:"root_directory_attributes"`
	DumpRestricted          bool                `yaml:"dump_restricted"`
	DumpAllowedClients      []string            `yaml:"dump_allowed_clients"`
}

type IdentityMapping struct {
	MapAllToAnonymous        bool  `yaml:"map_all_to_anonymous"`
	MapPrivilegedToAnonymous bool  `yaml:"map_privileged_to_anonymous"`
	AnonymousUID             int32 `yaml:"anonymous_uid"`
	AnonymousGID             int32 `yaml:"anonymous_gid"`
}

type DirectoryAttributes struct {
	Mode int32 `yaml:"mode"`
	UID  int32 `yaml:"uid"`
	GID  int32 `yaml:"gid"`
}

type AdaptersConfig struct {
	NFS NFSAdapter `yaml:"nfs"`
	SMB SMBAdapter `yaml:"smb,omitempty"`
}

type NFSAdapter struct {
	Enabled            bool           `yaml:"enabled"`
	Port               int32          `yaml:"port"`
	MaxConnections     int32          `yaml:"max_connections"`
	Timeouts           TimeoutsConfig `yaml:"timeouts"`
	MetricsLogInterval string         `yaml:"metrics_log_interval"`
}

type SMBAdapter struct {
	Enabled                  bool           `yaml:"enabled"`
	Port                     int32          `yaml:"port"`
	MaxConnections           int32          `yaml:"max_connections"`
	MaxRequestsPerConnection int32          `yaml:"max_requests_per_connection"`
	Timeouts                 TimeoutsConfig `yaml:"timeouts"`
	MetricsLogInterval       string         `yaml:"metrics_log_interval"`
	Credits                  *SMBCredits    `yaml:"credits,omitempty"`
}

type SMBCredits struct {
	Strategy                  string `yaml:"strategy"`
	MinGrant                  int32  `yaml:"min_grant"`
	MaxGrant                  int32  `yaml:"max_grant"`
	InitialGrant              int32  `yaml:"initial_grant"`
	MaxSessionCredits         int32  `yaml:"max_session_credits"`
	LoadThresholdHigh         int32  `yaml:"load_threshold_high,omitempty"`
	LoadThresholdLow          int32  `yaml:"load_threshold_low,omitempty"`
	AggressiveClientThreshold int32  `yaml:"aggressive_client_threshold,omitempty"`
}

type TimeoutsConfig struct {
	Read     string `yaml:"read"`
	Write    string `yaml:"write"`
	Idle     string `yaml:"idle"`
	Shutdown string `yaml:"shutdown"`
}

type User struct {
	Username         string            `yaml:"username"`
	PasswordHash     string            `yaml:"password_hash"`
	Enabled          bool              `yaml:"enabled"`
	UID              uint32            `yaml:"uid"`
	GID              uint32            `yaml:"gid"`
	Groups           []string          `yaml:"groups,omitempty"`
	SharePermissions map[string]string `yaml:"share_permissions,omitempty"`
}

type Group struct {
	Name             string            `yaml:"name"`
	GID              uint32            `yaml:"gid"`
	SharePermissions map[string]string `yaml:"share_permissions,omitempty"`
}

type Guest struct {
	Enabled          bool              `yaml:"enabled"`
	UID              uint32            `yaml:"uid"`
	GID              uint32            `yaml:"gid"`
	SharePermissions map[string]string `yaml:"share_permissions,omitempty"`
}
