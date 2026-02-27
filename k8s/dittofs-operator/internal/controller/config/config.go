package config

import (
	"fmt"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

// Default configuration values
const (
	DefaultLoggingLevel    = "INFO"
	DefaultLoggingFormat   = "json"
	DefaultLoggingOutput   = "stdout"
	DefaultShutdownTimeout = "30s"
	DefaultSQLitePath      = "/data/controlplane/controlplane.db"
	DefaultAPIPort         = 8080
	DefaultAccessDuration  = "15m"
	DefaultRefreshDuration = "168h" // 7 days
	DefaultJWTIssuer       = "dittofs"
	DefaultCachePath       = "/data/cache"
	DefaultCacheSize       = "1GB"
	DefaultAdminUsername   = "admin"
)

// GenerateDittoFSConfig generates DittoFS configuration YAML from the CRD spec.
// This generates infrastructure-only config matching the DittoFS develop branch format.
// Secrets (JWT, admin password, Postgres DSN) are NOT included in the config YAML.
// They are injected as environment variables sourced from Kubernetes Secrets instead.
// Dynamic configuration (stores, shares, users, adapters) is managed via REST API.
func GenerateDittoFSConfig(dittoServer *dittoiov1alpha1.DittoServer) (string, error) {
	// Build config without any secrets
	cfg := DittoFSConfig{
		Logging: LoggingConfig{
			Level:  DefaultLoggingLevel,
			Format: DefaultLoggingFormat,
			Output: DefaultLoggingOutput,
		},
		ShutdownTimeout: DefaultShutdownTimeout,
		Database:        buildDatabaseConfig(dittoServer),
		ControlPlane:    buildControlPlaneConfig(dittoServer),
		Cache:           buildCacheConfig(dittoServer),
	}

	// Add admin config with username only (password hash injected via env var)
	adminUsername := DefaultAdminUsername
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.Admin != nil &&
		dittoServer.Spec.Identity.Admin.Username != "" {
		adminUsername = dittoServer.Spec.Identity.Admin.Username
	}
	cfg.Admin = AdminConfig{
		Username: adminUsername,
	}

	yamlBytes, err := yaml.Marshal(&cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal config to YAML: %w", err)
	}

	return string(yamlBytes), nil
}

// buildDatabaseConfig constructs database configuration.
// Per CONTEXT.md: If PostgresSecretRef is set, Postgres takes precedence silently (regardless of Type field).
// The PostgreSQL connection string is NOT included - it is injected via environment variable.
func buildDatabaseConfig(ds *dittoiov1alpha1.DittoServer) DatabaseConfig {
	// Default to SQLite
	cfg := DatabaseConfig{
		Type: "sqlite",
		SQLite: &SQLiteConfig{
			Path: DefaultSQLitePath,
		},
	}

	if ds.Spec.Database == nil {
		return cfg
	}

	// Check for Postgres FIRST - takes precedence per CONTEXT.md
	// We check PostgresSecretRef being set as the indicator that Postgres is configured,
	// regardless of what Type field says. This implements "Postgres takes precedence silently".
	if ds.Spec.Database.PostgresSecretRef != nil {
		// PostgreSQL configured - include placeholder fields so Viper registers the keys
		// and can override them with DITTOFS_DATABASE_POSTGRES_* env vars from K8s Secrets.
		cfg.Type = "postgres"
		cfg.SQLite = nil
		cfg.Postgres = &PostgresConfig{
			Host:     "placeholder",
			Database: "placeholder",
			User:     "placeholder",
			Password: "placeholder",
		}
		return cfg
	}

	// Postgres not configured - use SQLite settings
	if ds.Spec.Database.Type == "sqlite" || ds.Spec.Database.Type == "" {
		if ds.Spec.Database.SQLite != nil && ds.Spec.Database.SQLite.Path != "" {
			cfg.SQLite.Path = ds.Spec.Database.SQLite.Path
		}
	}

	return cfg
}

// buildControlPlaneConfig constructs control plane API configuration.
// The JWT secret is NOT included - it is injected via environment variable.
func buildControlPlaneConfig(ds *dittoiov1alpha1.DittoServer) ControlPlaneConfig {
	port := DefaultAPIPort
	if ds.Spec.ControlPlane != nil && ds.Spec.ControlPlane.Port > 0 {
		port = int(ds.Spec.ControlPlane.Port)
	}

	jwtCfg := getJWTConfig(ds)

	return ControlPlaneConfig{
		Port: port,
		JWT: JWTConfig{
			Issuer:               stringOrDefault(jwtCfg.Issuer, DefaultJWTIssuer),
			AccessTokenDuration:  stringOrDefault(jwtCfg.AccessTokenDuration, DefaultAccessDuration),
			RefreshTokenDuration: stringOrDefault(jwtCfg.RefreshTokenDuration, DefaultRefreshDuration),
		},
	}
}

// getJWTConfig returns the JWT config from the spec, or an empty struct if not configured.
func getJWTConfig(ds *dittoiov1alpha1.DittoServer) *dittoiov1alpha1.JWTConfig {
	if ds.Spec.Identity != nil && ds.Spec.Identity.JWT != nil {
		return ds.Spec.Identity.JWT
	}
	return &dittoiov1alpha1.JWTConfig{}
}

// stringOrDefault returns the value if non-empty, otherwise returns the default.
func stringOrDefault(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

// buildCacheConfig constructs cache configuration
func buildCacheConfig(ds *dittoiov1alpha1.DittoServer) CacheConfig {
	cfg := CacheConfig{
		Path: DefaultCachePath,
		Size: DefaultCacheSize,
	}

	if ds.Spec.Cache == nil {
		return cfg
	}

	if ds.Spec.Cache.Path != "" {
		cfg.Path = ds.Spec.Cache.Path
	}
	if ds.Spec.Cache.Size != "" {
		cfg.Size = ds.Spec.Cache.Size
	}

	return cfg
}
