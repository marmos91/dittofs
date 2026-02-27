package config

// DittoFSConfig represents the DittoFS configuration matching develop branch format.
// This is infrastructure-only config - stores, shares, adapters, users are managed via REST API.
type DittoFSConfig struct {
	Logging         LoggingConfig      `yaml:"logging"`
	ShutdownTimeout string             `yaml:"shutdown_timeout"`
	Database        DatabaseConfig     `yaml:"database"`
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

// DatabaseConfig configures the control plane database
type DatabaseConfig struct {
	Type     string          `yaml:"type"`
	SQLite   *SQLiteConfig   `yaml:"sqlite,omitempty"`
	Postgres *PostgresConfig `yaml:"postgres,omitempty"`
}

// SQLiteConfig configures SQLite database
type SQLiteConfig struct {
	Path string `yaml:"path"`
}

// PostgresConfig configures PostgreSQL database.
// Values are placeholders overridden by env vars from Kubernetes Secrets.
type PostgresConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port,omitempty"`
	Database string `yaml:"database"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"sslmode,omitempty"`
}

// ControlPlaneConfig configures the control plane REST API
type ControlPlaneConfig struct {
	Port int       `yaml:"port"`
	JWT  JWTConfig `yaml:"jwt"`
}

// JWTConfig configures JWT authentication
type JWTConfig struct {
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
	Username string `yaml:"username"`
	Email    string `yaml:"email,omitempty"`
}
