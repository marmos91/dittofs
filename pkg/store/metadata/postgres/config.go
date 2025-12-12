package postgres

import (
	"fmt"
	"time"
)

// PostgresMetadataStoreConfig holds the configuration for PostgreSQL metadata store
type PostgresMetadataStoreConfig struct {
	// Connection parameters
	Host     string `mapstructure:"host" validate:"required"`
	Port     int    `mapstructure:"port" validate:"required"`
	Database string `mapstructure:"database" validate:"required"`
	User     string `mapstructure:"user" validate:"required"`
	Password string `mapstructure:"password" validate:"required"`
	SSLMode  string `mapstructure:"ssl_mode" validate:"oneof=disable require verify-ca verify-full"`

	// Connection Pool (conservative sizing)
	MaxConns          int32         `mapstructure:"max_conns"`           // Default: 10
	MinConns          int32         `mapstructure:"min_conns"`           // Default: 3
	MaxConnLifetime   time.Duration `mapstructure:"max_conn_lifetime"`   // Default: 1h
	MaxConnIdleTime   time.Duration `mapstructure:"max_conn_idle_time"`  // Default: 30m
	HealthCheckPeriod time.Duration `mapstructure:"health_check_period"` // Default: 1m

	// Timeouts
	ConnectTimeout time.Duration `mapstructure:"connect_timeout"` // Default: 5s
	QueryTimeout   time.Duration `mapstructure:"query_timeout"`   // Default: 30s

	// Features
	PrepareStatements bool          `mapstructure:"prepare_statements"` // Default: true
	AutoMigrate       bool          `mapstructure:"auto_migrate"`       // Default: false (manual control)
	StatsCacheTTL     time.Duration `mapstructure:"stats_cache_ttl"`    // Default: 5s
}

// ApplyDefaults sets default values for unspecified configuration fields
func (c *PostgresMetadataStoreConfig) ApplyDefaults() {
	// Connection pool defaults (conservative sizing)
	if c.MaxConns == 0 {
		c.MaxConns = 10
	}
	if c.MinConns == 0 {
		c.MinConns = 3
	}
	if c.MaxConnLifetime == 0 {
		c.MaxConnLifetime = 1 * time.Hour
	}
	if c.MaxConnIdleTime == 0 {
		c.MaxConnIdleTime = 30 * time.Minute
	}
	if c.HealthCheckPeriod == 0 {
		c.HealthCheckPeriod = 1 * time.Minute
	}

	// Timeout defaults
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 5 * time.Second
	}
	if c.QueryTimeout == 0 {
		c.QueryTimeout = 30 * time.Second
	}

	// Feature defaults
	// PrepareStatements defaults to false (Go zero value), but we want true
	// So we'll explicitly set it in the factory function if not specified by user

	if c.StatsCacheTTL == 0 {
		c.StatsCacheTTL = 5 * time.Second
	}

	// SSL mode default
	if c.SSLMode == "" {
		c.SSLMode = "prefer"
	}
}

// Validate checks if the configuration is valid
func (c *PostgresMetadataStoreConfig) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("host is required")
	}
	if c.Port == 0 {
		return fmt.Errorf("port is required")
	}
	if c.Database == "" {
		return fmt.Errorf("database is required")
	}
	if c.User == "" {
		return fmt.Errorf("user is required")
	}
	if c.Password == "" {
		return fmt.Errorf("password is required")
	}

	// Validate connection pool values
	if c.MaxConns < 1 {
		return fmt.Errorf("max_conns must be at least 1")
	}
	if c.MinConns < 0 {
		return fmt.Errorf("min_conns cannot be negative")
	}
	if c.MinConns > c.MaxConns {
		return fmt.Errorf("min_conns (%d) cannot be greater than max_conns (%d)", c.MinConns, c.MaxConns)
	}

	// Validate SSL mode
	validSSLModes := map[string]bool{
		"disable":     true,
		"require":     true,
		"verify-ca":   true,
		"verify-full": true,
		"prefer":      true,
	}
	if !validSSLModes[c.SSLMode] {
		return fmt.Errorf("invalid ssl_mode: %s (must be one of: disable, require, verify-ca, verify-full, prefer)", c.SSLMode)
	}

	return nil
}

// ConnectionString builds a PostgreSQL connection string from the config
func (c *PostgresMetadataStoreConfig) ConnectionString() string {
	return fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=%d",
		c.Host,
		c.Port,
		c.Database,
		c.User,
		c.Password,
		c.SSLMode,
		int(c.ConnectTimeout.Seconds()),
	)
}
