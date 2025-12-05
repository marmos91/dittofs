package config

// DittoFSConfig represents the complete DittoFS configuration structure
type DittoFSConfig struct {
	Logging  LoggingConfig  `yaml:"logging"`
	Server   ServerConfig   `yaml:"server"`
	Metadata MetadataConfig `yaml:"metadata"`
	Content  ContentConfig  `yaml:"content"`
	Shares   []ShareYAML    `yaml:"shares"`
	Adapters AdaptersConfig `yaml:"adapters"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	Output string `yaml:"output"`
}

type ServerConfig struct {
	ShutdownTimeout string `yaml:"shutdown_timeout"`
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
	Type   string                 `yaml:"type"`
	Badger *BadgerConfig          `yaml:"badger,omitempty"`
	Config map[string]interface{} `yaml:",inline,omitempty"`
}

type BadgerConfig struct {
	DBPath string `yaml:"db_path"`
}

type ContentConfig struct {
	Global map[string]interface{}  `yaml:"global"`
	Stores map[string]ContentStore `yaml:"stores"`
}

type ContentStore struct {
	Type       string                 `yaml:"type"`
	Filesystem *FilesystemConfig      `yaml:"filesystem,omitempty"`
	S3         *S3Config              `yaml:"s3,omitempty"`
	Config     map[string]interface{} `yaml:",inline,omitempty"`
}

type FilesystemConfig struct {
	Path string `yaml:"path"`
}

type S3Config struct {
	Bucket   string `yaml:"bucket,omitempty"`
	Region   string `yaml:"region,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

type ShareYAML struct {
	Name                    string              `yaml:"name"`
	MetadataStore           string              `yaml:"metadata_store"`
	ContentStore            string              `yaml:"content_store"`
	ReadOnly                bool                `yaml:"read_only"`
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
}

type NFSAdapter struct {
	Enabled            bool           `yaml:"enabled"`
	Port               int32          `yaml:"port"`
	MaxConnections     int32          `yaml:"max_connections"`
	Timeouts           TimeoutsConfig `yaml:"timeouts"`
	MetricsLogInterval string         `yaml:"metrics_log_interval"`
}

type TimeoutsConfig struct {
	Read     string `yaml:"read"`
	Write    string `yaml:"write"`
	Idle     string `yaml:"idle"`
	Shutdown string `yaml:"shutdown"`
}
