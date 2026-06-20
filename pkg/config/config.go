package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	vipermapstructure "github.com/go-viper/mapstructure/v2"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/nfs/identity"
	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config represents the DittoFS configuration.
//
// This structure captures static configuration aspects of the DittoFS server:
//   - Logging configuration
//   - Server settings (shutdown timeout, API)
//   - Database connection (control plane persistence)
//   - Admin user setup (for initial bootstrap)
//
// Block store sizing (cache, syncer) is auto-deduced from system resources
// at startup. Per-share overrides can be configured via dfsctl.
//
// Dynamic configuration (users, groups, shares, stores, adapters) is managed
// through the REST API and stored in the control plane database.
//
// Configuration sources (in order of precedence):
//  1. CLI flags (highest priority)
//  2. Environment variables (DITTOFS_*)
//  3. Configuration file (YAML or TOML)
//  4. Default values (lowest priority)
type Config struct {
	// Logging controls log output behavior
	Logging LoggingConfig `mapstructure:"logging" yaml:"logging"`

	// ShutdownTimeout is the maximum time to wait for graceful shutdown
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout" validate:"required,gt=0" yaml:"shutdown_timeout"`

	// Database configures the control plane database (SQLite or PostgreSQL).
	// This is the persistent store for users, groups, shares, and configuration.
	Database store.Config `mapstructure:"database" yaml:"database"`

	// ControlPlane contains control plane API server configuration
	ControlPlane api.APIConfig `mapstructure:"controlplane" yaml:"controlplane"`

	// Admin contains initial admin user configuration for bootstrap
	// This is used by 'dittofs init' to set up the first admin user
	Admin AdminConfig `mapstructure:"admin" yaml:"admin"`

	// Kerberos contains Kerberos/RPCSEC_GSS authentication configuration.
	// When enabled, NFS clients can authenticate using Kerberos tickets
	// via the RPCSEC_GSS protocol (RFC 2203).
	// Environment variable overrides:
	//   DITTOFS_KERBEROS_KEYTAB overrides KeytabPath (DITTOFS_KERBEROS_KEYTAB_PATH for compat)
	//   DITTOFS_KERBEROS_PRINCIPAL overrides ServicePrincipal (DITTOFS_KERBEROS_SERVICE_PRINCIPAL for compat)
	Kerberos KerberosConfig `mapstructure:"kerberos" yaml:"kerberos"`

	// Blockstore configures local/remote blockstore tunables.
	Blockstore BlockstoreConfig `mapstructure:"blockstore" yaml:"blockstore"`

	// GC configures the engine.CollectGarbage mark-sweep run.
	// These knobs apply globally to every block-store GC invocation.
	GC GCConfig `mapstructure:"gc" yaml:"gc"`

	// Snapshot configures the snapshot create orchestration. Knobs apply
	// to every Runtime.CreateSnapshot invocation.
	Snapshot SnapshotConfig `mapstructure:"snapshot" yaml:"snapshot"`

	// Metrics configures the Prometheus metrics endpoint (opt-in, disabled by
	// default). See MetricsConfig.
	Metrics MetricsConfig `mapstructure:"metrics" yaml:"metrics"`

	// Identity configures Windows/SID identity mapping. See IdentityConfig.
	Identity IdentityConfig `mapstructure:"identity" yaml:"identity"`
}

// IdentityConfig configures Windows/SID identity behavior shared across NFS,
// SMB, and the control plane.
//
// Environment variable override:
//
//	DITTOFS_IDENTITY_MACHINE_SID pins the machine SID.
type IdentityConfig struct {
	// MachineSID, when set, pins this node's machine SID to a fixed
	// "S-1-5-21-{a}-{b}-{c}" value instead of generating a random one on
	// first boot. Pin the SAME value on every node in a cluster so they
	// derive IDENTICAL local/algorithmic SIDs from the same Unix UID/GID
	// (the RID formula in pkg/auth/sid/mapper.go is a LOCKED, node-shared
	// invariant). Leave empty for a single node — the SID is then generated
	// once and persisted, staying stable across restarts.
	MachineSID string `mapstructure:"machine_sid" yaml:"machine_sid"`
}

// Validate returns an error if the IdentityConfig has invalid values.
func (c *IdentityConfig) Validate() error {
	if c.MachineSID == "" {
		return nil
	}
	if _, err := sid.NewSIDMapperFromString(c.MachineSID); err != nil {
		return fmt.Errorf("identity.machine_sid %q is invalid: %w", c.MachineSID, err)
	}
	return nil
}

// GCConfig configures the engine.CollectGarbage mark-sweep run. Knobs
// cover the grace TTL and the dry-run sample bound; GC is on-demand only
// (dfsctl/REST) with no periodic scheduler.
type GCConfig struct {
	// GracePeriod is the TTL applied during sweep: an object whose
	// LastModified is within snapshot - GracePeriod is preserved.
	// Defaults to 1 hour. Values in (0, 5m) are rejected at config load;
	// values in [5m, 10m) are accepted but emit a warning.
	GracePeriod time.Duration `mapstructure:"grace_period" yaml:"grace_period"`

	// DryRunSampleSize bounds the number of candidate keys captured by a
	// dry-run report. Defaults to 1000.
	DryRunSampleSize int `mapstructure:"dry_run_sample_size" yaml:"dry_run_sample_size"`
}

// ApplyDefaults fills any zero-valued field with the defaults.
func (c *GCConfig) ApplyDefaults() {
	if c.GracePeriod <= 0 {
		c.GracePeriod = time.Hour
	}
	if c.DryRunSampleSize <= 0 {
		c.DryRunSampleSize = 1000
	}
}

// Validate returns an error if the GCConfig has invalid values.
//
// GracePeriod: zero is allowed (the engine substitutes the 1h default in
// ApplyDefaults / engine.Options). Any positive value below 5m is
// rejected: server-S3 clock skew under sustained load can easily exceed
// a few minutes, and a sub-5m grace TTL collapses the snapshot-grace
// contract that protects in-flight CAS PUTs from being reaped on the
// same sweep. Values in [5m, 10m) are accepted but emit a warn —
// they're inside spec but tighter than the recommended floor.
func (c *GCConfig) Validate() error {
	if c.GracePeriod < 0 {
		return fmt.Errorf("gc.grace_period must be >= 0 (got %v)", c.GracePeriod)
	}
	if c.GracePeriod > 0 && c.GracePeriod < 5*time.Minute {
		return fmt.Errorf("gc.grace_period must be >= 5m to absorb server/S3 clock skew (got %v); set 0 to use the 1h default", c.GracePeriod)
	}
	if c.GracePeriod >= 5*time.Minute && c.GracePeriod < 10*time.Minute {
		logger.Warn("gc.grace_period is below the recommended 10m floor; CAS PUTs racing the mark phase may be reaped if clock skew exceeds the configured window",
			"configured", c.GracePeriod)
	}
	if c.DryRunSampleSize < 0 {
		return fmt.Errorf("gc.dry_run_sample_size must be >= 0 (got %d)", c.DryRunSampleSize)
	}
	return nil
}

// SnapshotConfig configures the snapshot create orchestration and the
// REST handler that wraps Runtime.RestoreSnapshot.
type SnapshotConfig struct {
	// RestoreHTTPTimeout bounds the per-request context wrapping
	// Runtime.RestoreSnapshot in the REST handler. Defaults to 30
	// minutes when unset.
	RestoreHTTPTimeout time.Duration `mapstructure:"restore_http_timeout" yaml:"restore_http_timeout"`

	// SchedulerPollInterval is the cadence at which the background snapshot
	// scheduler scans policies for due snapshots. Defaults to one minute when
	// unset. The per-share policy interval (not this knob) controls how often
	// a given share is actually snapshotted.
	SchedulerPollInterval time.Duration `mapstructure:"scheduler_poll_interval" yaml:"scheduler_poll_interval"`

	// SchedulerDisabled turns the background snapshot scheduler off entirely.
	// Per-share policies are still persisted and can be run manually, but no
	// automatic create/prune occurs while disabled.
	SchedulerDisabled bool `mapstructure:"scheduler_disabled" yaml:"scheduler_disabled"`
}

// DefaultRestoreHTTPTimeout is the fallback applied by ApplyDefaults when
// SnapshotConfig.RestoreHTTPTimeout is zero.
const DefaultRestoreHTTPTimeout = 30 * time.Minute

// DefaultSchedulerPollInterval is the fallback applied by ApplyDefaults when
// SnapshotConfig.SchedulerPollInterval is zero.
const DefaultSchedulerPollInterval = time.Minute

// ApplyDefaults fills any zero-valued field with the defaults.
func (c *SnapshotConfig) ApplyDefaults() {
	if c.RestoreHTTPTimeout == 0 {
		c.RestoreHTTPTimeout = DefaultRestoreHTTPTimeout
	}
	if c.SchedulerPollInterval == 0 {
		c.SchedulerPollInterval = DefaultSchedulerPollInterval
	}
}

// Validate returns an error if the SnapshotConfig has invalid values.
func (c *SnapshotConfig) Validate() error {
	if c.RestoreHTTPTimeout < 0 {
		return fmt.Errorf("snapshot.restore_http_timeout must be >= 0 (got %s)", c.RestoreHTTPTimeout)
	}
	if c.SchedulerPollInterval < 0 {
		return fmt.Errorf("snapshot.scheduler_poll_interval must be >= 0 (got %s)", c.SchedulerPollInterval)
	}
	return nil
}

// LoggingConfig controls logging behavior.
type LoggingConfig struct {
	// Level is the minimum log level to output
	// Valid values: DEBUG, INFO, WARN, ERROR (case-insensitive, normalized to uppercase)
	Level string `mapstructure:"level" validate:"required,oneof=DEBUG INFO WARN ERROR debug info warn error" yaml:"level"`

	// Format specifies the log output format
	// Valid values: text, json
	Format string `mapstructure:"format" validate:"required,oneof=text json" yaml:"format"`

	// Output specifies where logs are written
	// Valid values: stdout, stderr, or a file path
	Output string `mapstructure:"output" validate:"required" yaml:"output"`

	// Rotation configures log file rotation (only active when Output is a file path)
	Rotation LogRotationConfig `mapstructure:"rotation" yaml:"rotation"`
}

// LogRotationConfig controls log file rotation via lumberjack.
// Rotation is only active when logging output is a file path (not stdout/stderr).
type LogRotationConfig struct {
	// MaxSize is the maximum size in megabytes of the log file before it gets rotated.
	// If MaxSize is 0, size-based rotation is disabled; if greater than 0, rotation
	// occurs when the file exceeds this size. The defaults layer sets this to 100 MB.
	MaxSize int `mapstructure:"max_size" yaml:"max_size"`

	// MaxBackups is the maximum number of old log files to retain.
	// 0 means keep all old log files.
	// The generated config template sets this to 5.
	MaxBackups int `mapstructure:"max_backups" yaml:"max_backups"`

	// MaxAge is the maximum number of days to retain old log files.
	// 0 means no age limit (keep forever).
	// The generated config template sets this to 30.
	MaxAge int `mapstructure:"max_age" yaml:"max_age"`

	// Compress determines whether rotated log files are gzip compressed.
	// Default: false
	Compress bool `mapstructure:"compress" yaml:"compress"`
}

// AdminConfig contains initial admin user configuration for bootstrap.
// This is used by 'dittofs init' to pre-configure the first admin user.
type AdminConfig struct {
	// Username is the admin username
	// Default: "admin"
	Username string `mapstructure:"username" yaml:"username"`

	// Email is the admin user's email address (optional)
	Email string `mapstructure:"email" yaml:"email,omitempty"`

	// PasswordHash is the bcrypt hash of the admin password
	// Generated during 'dittofs init' or can be set manually
	// Use: htpasswd -nbB "" "password" | cut -d: -f2
	PasswordHash string `mapstructure:"password_hash" yaml:"password_hash,omitempty"`
}

// redactedSecret is the sentinel substituted for sensitive fields when the
// config is serialized for display (e.g. `dfs config show`). It is never
// parsed back on the load path, which uses mapstructure/viper rather than
// json/yaml Unmarshal of these types.
const redactedSecret = "********"

// MarshalYAML redacts the admin password hash when the config is serialized
// for display. An empty hash stays empty so "unset" is distinguishable from a
// redacted value.
func (c AdminConfig) MarshalYAML() (interface{}, error) {
	type alias AdminConfig // avoid infinite recursion
	out := alias(c)
	if out.PasswordHash != "" {
		out.PasswordHash = redactedSecret
	}
	return out, nil
}

// MarshalJSON redacts the admin password hash when the config is serialized
// for display. See MarshalYAML.
func (c AdminConfig) MarshalJSON() ([]byte, error) {
	type alias AdminConfig // avoid infinite recursion
	out := alias(c)
	if out.PasswordHash != "" {
		out.PasswordHash = redactedSecret
	}
	return json.Marshal(out)
}

// KerberosConfig contains Kerberos/RPCSEC_GSS authentication configuration.
//
// When Enabled is true, the NFS server supports Kerberos authentication
// via RPCSEC_GSS (RFC 2203). Clients can authenticate using krb5, krb5i
// (integrity), or krb5p (privacy) security flavors.
//
// The server needs a keytab file containing the service principal's key
// and a valid krb5.conf for realm/KDC resolution.
type KerberosConfig struct {
	// Enabled controls whether Kerberos authentication is active.
	// Default: false (AUTH_UNIX only)
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// KeytabPath is the path to the Kerberos keytab file.
	// The keytab must contain the service principal's key.
	// Override: DITTOFS_KERBEROS_KEYTAB (primary), DITTOFS_KERBEROS_KEYTAB_PATH (compat)
	// Example: /etc/dittofs/dittofs.keytab
	KeytabPath string `mapstructure:"keytab_path" yaml:"keytab_path"`

	// ServicePrincipal is the Kerberos service principal name (SPN).
	// Format: service/hostname@REALM (e.g., nfs/server.example.com@EXAMPLE.COM)
	// Override: DITTOFS_KERBEROS_PRINCIPAL (primary), DITTOFS_KERBEROS_SERVICE_PRINCIPAL (compat)
	ServicePrincipal string `mapstructure:"service_principal" yaml:"service_principal"`

	// Krb5Conf is the path to the Kerberos configuration file.
	// Default: /etc/krb5.conf
	Krb5Conf string `mapstructure:"krb5_conf" yaml:"krb5_conf"`

	// MaxClockSkew is the maximum allowed clock difference between client and server.
	// Kerberos requires synchronized clocks; this tolerance handles minor drift.
	// Default: 5m
	MaxClockSkew time.Duration `mapstructure:"max_clock_skew" yaml:"max_clock_skew"`

	// ContextTTL is the maximum lifetime of an RPCSEC_GSS security context.
	// After this duration, clients must re-authenticate.
	// Default: 8h
	ContextTTL time.Duration `mapstructure:"context_ttl" yaml:"context_ttl"`

	// MaxContexts is the maximum number of concurrent RPCSEC_GSS contexts.
	// Prevents memory exhaustion from excessive context creation.
	// Default: 10000
	MaxContexts int `mapstructure:"max_contexts" yaml:"max_contexts"`

	// IdentityMapping configures how Kerberos principals are mapped to Unix identities.
	IdentityMapping IdentityMappingConfig `mapstructure:"identity_mapping" yaml:"identity_mapping"`
}

// Validate returns an error if the KerberosConfig has invalid values.
//
// Negative durations/counts are rejected (0 means "use the default", applied
// in ApplyDefaults): a negative ContextTTL would expire every live GSS context
// on each cleanup sweep (auth-availability churn), and a negative MaxClockSkew
// would reject otherwise-valid tickets. The identity-mapping Strategy is
// validated against the supported set (only "static" is implemented;
// BuildStaticMapper ignores any other value, so an unvalidated typo silently
// degrades to static mapping).
func (c *KerberosConfig) Validate() error {
	if c.ContextTTL < 0 {
		return fmt.Errorf("kerberos.context_ttl must be >= 0 (got %v); 0 uses the default", c.ContextTTL)
	}
	if c.MaxClockSkew < 0 {
		return fmt.Errorf("kerberos.max_clock_skew must be >= 0 (got %v); 0 uses the default", c.MaxClockSkew)
	}
	if c.MaxContexts < 0 {
		return fmt.Errorf("kerberos.max_contexts must be >= 0 (got %d); 0 uses the default", c.MaxContexts)
	}
	switch c.IdentityMapping.Strategy {
	case "", "static":
		// "" is normalized to "static" in ApplyDefaults.
	default:
		return fmt.Errorf("kerberos.identity_mapping.strategy %q is not supported (only \"static\")", c.IdentityMapping.Strategy)
	}
	return nil
}

// IdentityMappingConfig controls how Kerberos principals are mapped to Unix UID/GID.
//
// The mapping strategy determines how authenticated Kerberos principals
// (e.g., "alice@EXAMPLE.COM") are converted to Unix identities for
// NFS file permission checks.
type IdentityMappingConfig struct {
	// Strategy selects the identity mapping approach.
	// Currently supported: "static" (map from config file)
	// Future: "ldap", "nsswitch", "regex"
	// Default: "static"
	Strategy string `mapstructure:"strategy" yaml:"strategy"`

	// StaticMap maps "principal@REALM" strings to Unix identities.
	// Only used when Strategy is "static".
	// Example: {"alice@EXAMPLE.COM": {UID: 1000, GID: 1000}}
	StaticMap map[string]StaticIdentity `mapstructure:"static_map" yaml:"static_map"`

	// DefaultUID is the Unix UID assigned to principals not found in StaticMap.
	// Default: 65534 (nobody)
	DefaultUID uint32 `mapstructure:"default_uid" yaml:"default_uid"`

	// DefaultGID is the Unix GID assigned to principals not found in StaticMap.
	// Default: 65534 (nogroup)
	DefaultGID uint32 `mapstructure:"default_gid" yaml:"default_gid"`
}

// StaticIdentity represents a Unix identity for a specific Kerberos principal.
type StaticIdentity struct {
	// UID is the Unix user ID
	UID uint32 `mapstructure:"uid" yaml:"uid"`

	// GID is the Unix primary group ID
	GID uint32 `mapstructure:"gid" yaml:"gid"`

	// GIDs is a list of supplementary group IDs
	GIDs []uint32 `mapstructure:"gids" yaml:"gids,omitempty"`
}

// BuildStaticMapper converts an IdentityMappingConfig to an identity.StaticMapper.
// This is the canonical conversion point between config types and identity types.
func BuildStaticMapper(idCfg *IdentityMappingConfig) *identity.StaticMapper {
	if idCfg == nil {
		return identity.NewStaticMapper(&identity.StaticMapperConfig{})
	}

	staticMap := make(map[string]identity.StaticIdentity, len(idCfg.StaticMap))
	for k, v := range idCfg.StaticMap {
		var gidsCopy []uint32
		if v.GIDs != nil {
			gidsCopy = make([]uint32, len(v.GIDs))
			copy(gidsCopy, v.GIDs)
		}
		staticMap[k] = identity.StaticIdentity{
			UID:  v.UID,
			GID:  v.GID,
			GIDs: gidsCopy,
		}
	}
	return identity.NewStaticMapper(&identity.StaticMapperConfig{
		StaticMap:  staticMap,
		DefaultUID: idCfg.DefaultUID,
		DefaultGID: idCfg.DefaultGID,
	})
}

// Load loads configuration from file, environment, and defaults.
//
// Configuration precedence (highest to lowest):
//  1. Environment variables (DITTOFS_*)
//  2. Configuration file
//  3. Default values
//
// Parameters:
//   - configPath: Path to config file (empty string uses default location)
//
// Returns:
//   - *Config: Loaded and validated configuration
//   - error: Configuration loading or validation error
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// Configure viper
	setupViper(v, configPath)

	// Read configuration file if it exists
	configFileFound, err := readConfigFile(v)
	if err != nil {
		return nil, err
	}

	// Unmarshal into config struct with custom decode hooks.
	//
	// This runs even when no config file was found. setupViper already bound
	// every DITTOFS_* env key, and v.Unmarshal is the only path that applies
	// those bindings; short-circuiting to GetDefaultConfig() on the no-file
	// path would silently drop every env override (e.g.
	// DITTOFS_DATABASE_TYPE=postgres, DITTOFS_CONTROLPLANE_SECRET) for
	// container/CI deployments that ship no config file, violating the
	// documented "env > file > defaults" precedence. Unmarshalling into a zero
	// struct then ApplyDefaults yields defaults for every field the env doesn't
	// set, identical to the file path.
	//
	// Capture unknown keys via decoder Metadata and log them as a warning
	// rather than silently dropping them — this surfaces typos and stale keys
	// from removed config trees (e.g. the deleted `lock:`/`syncer:` sections or
	// a `cache:` block) without hard-failing boot on an otherwise-valid config
	// that still carries a legacy key (upgrade safety).
	var md vipermapstructure.Metadata
	var cfg Config
	if err := v.Unmarshal(&cfg,
		viper.DecodeHook(configDecodeHooks()),
		func(dc *vipermapstructure.DecoderConfig) { dc.Metadata = &md },
	); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	// Unknown-key warnings only make sense when a file was actually parsed;
	// the env-only path has no file keys to be reported as unused.
	if configFileFound && len(md.Unused) > 0 {
		sort.Strings(md.Unused)
		fmt.Fprintf(os.Stderr, "WARNING: config contains unknown keys (ignored): %s\n", strings.Join(md.Unused, ", "))
	}

	// Apply defaults for any missing values
	ApplyDefaults(&cfg)

	// Validate configuration
	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return &cfg, nil
}

// MustLoad loads configuration with helpful error messages.
// It checks if the config file exists and provides user-friendly instructions if not.
//
// Parameters:
//   - configPath: Path to config file (empty string uses default location)
//
// Returns:
//   - *Config: Loaded and validated configuration
//   - error: User-friendly error with instructions if config not found
func MustLoad(configPath string) (*Config, error) {
	// Determine config path
	if configPath == "" {
		if !DefaultConfigExists() {
			return nil, fmt.Errorf("no configuration file found at default location: %s\n\n"+
				"Please initialize a configuration file first:\n"+
				"  dittofs init\n\n"+
				"Or specify a custom config file:\n"+
				"  dittofs <command> --config /path/to/config.yaml",
				GetDefaultConfigPath())
		}
		configPath = GetDefaultConfigPath()
	} else {
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("configuration file not found: %s\n\n"+
				"Please create the configuration file:\n"+
				"  dittofs init --config %s",
				configPath, configPath)
		}
	}

	// Load configuration
	cfg, err := Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	return cfg, nil
}

// SaveConfig saves the configuration to the specified file path.
// The configuration is saved in YAML format using proper yaml tags.
func SaveConfig(cfg *Config, path string) error {
	// Create parent directory if it doesn't exist
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Use yaml.Marshal directly to respect yaml tags
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write to file with restricted permissions (0600 = owner read/write only).
	// This is important because config files may contain sensitive data like password hashes.
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// setupViper configures viper with environment variables and config file settings.
func setupViper(v *viper.Viper, configPath string) {
	// Set up environment variable support
	// Environment variables use DITTOFS_ prefix and underscores
	// Example: DITTOFS_LOGGING_LEVEL=DEBUG
	v.SetEnvPrefix("DITTOFS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Bind every config key for AutomaticEnv resolution.
	//
	// Viper's AutomaticEnv + Unmarshal only honours an env var for a key
	// that is already known to viper (present in the config file, a
	// SetDefault, or an explicit BindEnv). Any key absent from the file
	// is otherwise silently dropped from the env — so e.g. a container
	// setting DITTOFS_DATABASE_TYPE=postgres with `type` omitted from the
	// file would be ignored and the server would silently boot on SQLite.
	//
	// To make env precedence reliable for the whole config surface we
	// reflect over the default Config struct and BindEnv every nested
	// mapstructure key path (dot-separated; the key replacer above maps
	// "." -> "_" so database.postgres.ssl_root_cert binds
	// DITTOFS_DATABASE_POSTGRES_SSL_ROOT_CERT).
	for _, key := range configEnvKeys() {
		_ = v.BindEnv(key)
	}

	// DITTOFS_CONTROLPLANE_SECRET is documented as a first-class override
	// for the JWT signing key. The struct field is JWTConfig.Secret nested
	// under controlplane.jwt, i.e. viper key controlplane.jwt.secret — so the
	// documented short form must bind to THAT key, not controlplane.secret
	// (which no struct field reads). Pass both the documented short form and
	// the auto long form explicitly: a second BindEnv overwrites the env-var
	// list set by the reflective configEnvKeys() walk (which bound the long
	// form DITTOFS_CONTROLPLANE_JWT_SECRET), so we must re-list it here to
	// keep both forms working.
	_ = v.BindEnv("controlplane.jwt.secret", "DITTOFS_CONTROLPLANE_SECRET", "DITTOFS_CONTROLPLANE_JWT_SECRET")

	// Configure config file search
	if configPath != "" {
		// Use explicitly specified config file
		v.SetConfigFile(configPath)
	} else {
		// Use default location: $XDG_CONFIG_HOME/dittofs/config.{yaml,toml}
		configDir := getConfigDir()
		v.AddConfigPath(configDir)
		v.SetConfigName("config")
		v.SetConfigType("yaml") // Primary format
	}
}

// configEnvKeys returns every dot-separated mapstructure key path in the
// Config struct, so each can be bound for AutomaticEnv resolution. Nested
// structs are walked recursively; leaf fields (including maps and slices)
// contribute their own key. Maps with dynamic keys (e.g. the Kerberos
// static_map) bind at the container level only — env cannot address
// arbitrary map entries.
func configEnvKeys() []string {
	var keys []string
	collectMapstructureKeys(reflect.TypeOf(Config{}), "", &keys)
	return keys
}

// collectMapstructureKeys recursively appends the mapstructure key paths of t
// (rooted at prefix) to out.
func collectMapstructureKeys(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		tag := field.Tag.Get("mapstructure")
		// Strip mapstructure options like ",squash"/",omitempty".
		name := tag
		if comma := strings.IndexByte(name, ','); comma >= 0 {
			name = name[:comma]
		}
		if name == "" {
			// Untagged exported field: skip — it has no stable key.
			continue
		}

		key := name
		if prefix != "" {
			key = prefix + "." + name
		}

		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}

		// Recurse into nested structs (but not time.Duration, which is a
		// named int64, nor stdlib structs we treat as leaves). A struct
		// with mapstructure-tagged fields is a nested namespace.
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Duration(0)) && hasMapstructureFields(ft) {
			collectMapstructureKeys(ft, key, out)
			continue
		}

		*out = append(*out, key)
	}
}

// hasMapstructureFields reports whether t has at least one exported field with
// a mapstructure tag, i.e. it is a config namespace worth recursing into.
func hasMapstructureFields(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.IsExported() && f.Tag.Get("mapstructure") != "" {
			return true
		}
	}
	return false
}

// readConfigFile reads the configuration file if it exists.
// Returns (fileFound, error) where fileFound indicates if a config file was found.
func readConfigFile(v *viper.Viper) (bool, error) {
	if err := v.ReadInConfig(); err != nil {
		// Check if error is "config file not found"
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found is acceptable - use defaults
			return false, nil
		}
		// Also check for os.PathError when explicit config file doesn't exist
		if os.IsNotExist(err) {
			// Config file not found is acceptable - use defaults
			return false, nil
		}
		// Other errors are problems
		return false, fmt.Errorf("failed to read config file: %w", err)
	}

	return true, nil
}

// configDecodeHooks returns a combined decode hook for all custom types.
// This includes ByteSize and time.Duration parsing.
func configDecodeHooks() mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		byteSizeDecodeHook(),
		durationDecodeHook(),
	)
}

// byteSizeDecodeHook returns a mapstructure decode hook that converts strings
// and integers to bytesize.ByteSize or *bytesize.ByteSize. This enables config
// files to use human-readable sizes like "1Gi", "500Mi", "100MB", or plain numbers.
// Pointer targets (*bytesize.ByteSize) are supported so that nil can represent
// "unset" while an explicit 0 means "disabled".
func byteSizeDecodeHook() mapstructure.DecodeHookFunc {
	byteSizeType := reflect.TypeOf(bytesize.ByteSize(0))
	byteSizePtrType := reflect.PointerTo(byteSizeType)

	return func(from reflect.Type, to reflect.Type, data interface{}) (interface{}, error) {
		isPtr := to == byteSizePtrType
		if !isPtr && to != byteSizeType {
			return data, nil
		}

		var result bytesize.ByteSize
		switch v := data.(type) {
		case string:
			parsed, err := bytesize.ParseByteSize(v)
			if err != nil {
				return nil, err
			}
			result = parsed
		case int:
			result = bytesize.ByteSize(v)
		case int64:
			result = bytesize.ByteSize(v)
		case uint64:
			result = bytesize.ByteSize(v)
		case float64:
			// YAML often deserializes numbers as float64
			result = bytesize.ByteSize(v)
		case bytesize.ByteSize:
			result = v
		case *bytesize.ByteSize:
			if v == nil {
				return data, nil
			}
			result = *v
		default:
			return data, nil
		}

		if isPtr {
			return &result, nil
		}
		return result, nil
	}
}

// durationDecodeHook returns a mapstructure decode hook that converts strings
// to time.Duration. This enables config files to use human-readable durations
// like "30s", "5m", "1h".
func durationDecodeHook() mapstructure.DecodeHookFunc {
	return func(from reflect.Type, to reflect.Type, data interface{}) (interface{}, error) {
		// Only handle conversion to time.Duration
		if to != reflect.TypeOf(time.Duration(0)) {
			return data, nil
		}

		switch v := data.(type) {
		case string:
			// Parse duration string like "30s", "5m", "1h"
			return time.ParseDuration(v)
		case int:
			// Assume nanoseconds for raw integers
			return time.Duration(v), nil
		case int64:
			return time.Duration(v), nil
		case float64:
			// YAML often deserializes numbers as float64
			return time.Duration(v), nil
		default:
			return data, nil
		}
	}
}

// getConfigDir returns the configuration directory path.
//
// On Windows, uses %APPDATA%\dittofs (matching internal/cli/credentials/store.go pattern).
// On Unix, uses XDG_CONFIG_HOME/dittofs or ~/.config/dittofs.
// Falls back to current directory (.) if home directory cannot be determined.
func getConfigDir() string {
	if runtime.GOOS == "windows" {
		// On Windows, use %APPDATA%\dittofs
		appData := os.Getenv("APPDATA")
		if appData != "" {
			return filepath.Join(appData, "dittofs")
		}
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, "AppData", "Roaming", "dittofs")
		}
		return "."
	}

	// Unix: XDG_CONFIG_HOME or ~/.config
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		return filepath.Join(xdgConfig, "dittofs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "dittofs")
}

// GetDefaultConfigPath returns the default configuration file path.
func GetDefaultConfigPath() string {
	return filepath.Join(getConfigDir(), "config.yaml")
}

// DefaultConfigExists checks if a config file exists at the default location.
func DefaultConfigExists() bool {
	path := GetDefaultConfigPath()
	_, err := os.Stat(path)
	return err == nil
}

// GetConfigDir returns the configuration directory path (exposed for init command).
func GetConfigDir() string {
	return getConfigDir()
}

// GetStateDir returns the state directory path for runtime data (logs, PID files).
//
// On Windows, uses %LOCALAPPDATA%\dittofs.
// On Unix, uses XDG_STATE_HOME/dittofs or ~/.local/state/dittofs.
func GetStateDir() string {
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData != "" {
			return filepath.Join(localAppData, "dittofs")
		}
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, "AppData", "Local", "dittofs")
		}
		return filepath.Join(os.TempDir(), "dittofs")
	}

	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "dittofs")
		}
		stateDir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateDir, "dittofs")
}

// GetDefaultLogPath returns the default log file path.
func GetDefaultLogPath() string {
	return filepath.Join(GetStateDir(), "dittofs.log")
}

// InitLogger initializes the structured logger from a LoggingConfig,
// including rotation settings. This is the canonical way to initialize
// the logger from configuration — prefer this over constructing
// logger.Config manually to ensure rotation settings are plumbed through.
func InitLogger(cfg *Config) error {
	loggerCfg := logger.Config{
		Level:      cfg.Logging.Level,
		Format:     cfg.Logging.Format,
		Output:     cfg.Logging.Output,
		MaxSize:    cfg.Logging.Rotation.MaxSize,
		MaxBackups: cfg.Logging.Rotation.MaxBackups,
		MaxAge:     cfg.Logging.Rotation.MaxAge,
		Compress:   cfg.Logging.Rotation.Compress,
	}
	if err := logger.Init(loggerCfg); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	return nil
}
