package config

// DittoFSConfig represents the DittoFS configuration matching develop branch format.
// This is infrastructure-only config - stores, shares, adapters, users are managed via REST API.
type DittoFSConfig struct {
	Logging         LoggingConfig      `yaml:"logging"`
	Telemetry       TelemetryConfig    `yaml:"telemetry,omitempty"`
	ShutdownTimeout string             `yaml:"shutdown_timeout"`
	Database        DatabaseConfig     `yaml:"database"`
	Metrics         MetricsConfig      `yaml:"metrics"`
	ControlPlane    ControlPlaneConfig `yaml:"controlplane"`
	Cache           CacheConfig        `yaml:"cache"`
	Admin           AdminConfig        `yaml:"admin,omitempty"`
}

// LoggingConfig controls logging behavior
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	Output string `yaml:"output"`
}

// TelemetryConfig controls OpenTelemetry tracing
type TelemetryConfig struct {
	Enabled    bool    `yaml:"enabled"`
	Endpoint   string  `yaml:"endpoint,omitempty"`
	Insecure   bool    `yaml:"insecure,omitempty"`
	SampleRate float64 `yaml:"sample_rate,omitempty"`
}

// DatabaseConfig configures the control plane database
type DatabaseConfig struct {
	Type     string        `yaml:"type"`
	SQLite   *SQLiteConfig `yaml:"sqlite,omitempty"`
	Postgres *string       `yaml:"postgres,omitempty"`
}

// SQLiteConfig configures SQLite database
type SQLiteConfig struct {
	Path string `yaml:"path"`
}

// MetricsConfig configures Prometheus metrics
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port,omitempty"`
}

// ControlPlaneConfig configures the control plane REST API
type ControlPlaneConfig struct {
	Port int       `yaml:"port"`
	JWT  JWTConfig `yaml:"jwt"`
}

// JWTConfig configures JWT authentication
type JWTConfig struct {
	Secret               string `yaml:"secret"`
	Issuer               string `yaml:"issuer,omitempty"`
	AccessTokenDuration  string `yaml:"access_token_duration"`
	RefreshTokenDuration string `yaml:"refresh_token_duration"`
}

// CacheConfig configures the WAL-backed cache
type CacheConfig struct {
	Path string `yaml:"path"`
	Size string `yaml:"size,omitempty"`
}

// AdminConfig configures the initial admin user
type AdminConfig struct {
	Username     string `yaml:"username"`
	Email        string `yaml:"email,omitempty"`
	PasswordHash string `yaml:"password_hash,omitempty"`
}
