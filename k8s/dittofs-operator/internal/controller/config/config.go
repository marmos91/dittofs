package config

import (
	"context"
	"fmt"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Default configuration values
const (
	DefaultLoggingLevel    = "INFO"
	DefaultLoggingFormat   = "json"
	DefaultLoggingOutput   = "stdout"
	DefaultShutdownTimeout = "30s"
	DefaultSQLitePath      = "/data/controlplane/controlplane.db"
	DefaultMetricsPort     = 9090
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
// Dynamic configuration (stores, shares, users, adapters) is managed via REST API.
func GenerateDittoFSConfig(ctx context.Context, c client.Client, dittoServer *dittoiov1alpha1.DittoServer) (string, error) {
	// Resolve JWT secret
	jwtSecret := ""
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.JWT != nil {
		secret, err := resolveSecretValue(ctx, c, dittoServer.Namespace, dittoServer.Spec.Identity.JWT.SecretRef)
		if err != nil {
			return "", fmt.Errorf("failed to resolve JWT secret: %w", err)
		}
		jwtSecret = secret
	}

	// Resolve admin password hash
	adminPasswordHash := ""
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.Admin != nil &&
		dittoServer.Spec.Identity.Admin.PasswordSecretRef != nil {
		hash, err := resolveSecretValue(ctx, c, dittoServer.Namespace, *dittoServer.Spec.Identity.Admin.PasswordSecretRef)
		if err != nil {
			return "", fmt.Errorf("failed to resolve admin password: %w", err)
		}
		adminPasswordHash = hash
	}

	// Build database config (resolves Postgres secret if configured)
	dbConfig, err := buildDatabaseConfig(ctx, c, dittoServer)
	if err != nil {
		return "", fmt.Errorf("failed to build database config: %w", err)
	}

	// Build config
	cfg := DittoFSConfig{
		Logging: LoggingConfig{
			Level:  DefaultLoggingLevel,
			Format: DefaultLoggingFormat,
			Output: DefaultLoggingOutput,
		},
		Telemetry: TelemetryConfig{
			Enabled: false,
		},
		ShutdownTimeout: DefaultShutdownTimeout,
		Database:        dbConfig,
		Metrics:         buildMetricsConfig(dittoServer),
		ControlPlane:    buildControlPlaneConfig(dittoServer, jwtSecret),
		Cache:           buildCacheConfig(dittoServer),
	}

	// Add admin config if we have credentials
	adminUsername := DefaultAdminUsername
	if dittoServer.Spec.Identity != nil && dittoServer.Spec.Identity.Admin != nil &&
		dittoServer.Spec.Identity.Admin.Username != "" {
		adminUsername = dittoServer.Spec.Identity.Admin.Username
	}
	if adminPasswordHash != "" {
		cfg.Admin = AdminConfig{
			Username:     adminUsername,
			PasswordHash: adminPasswordHash,
		}
	}

	yamlBytes, err := yaml.Marshal(&cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal config to YAML: %w", err)
	}

	return string(yamlBytes), nil
}

// buildDatabaseConfig constructs database configuration.
// Per CONTEXT.md: If PostgresSecretRef is set, Postgres takes precedence silently (regardless of Type field).
// PostgreSQL connection string is resolved from Secret and included in config YAML.
func buildDatabaseConfig(ctx context.Context, c client.Client, ds *dittoiov1alpha1.DittoServer) (DatabaseConfig, error) {
	// Default to SQLite
	cfg := DatabaseConfig{
		Type: "sqlite",
		SQLite: &SQLiteConfig{
			Path: DefaultSQLitePath,
		},
	}

	if ds.Spec.Database == nil {
		return cfg, nil
	}

	// Check for Postgres FIRST - takes precedence per CONTEXT.md
	// We check PostgresSecretRef being set as the indicator that Postgres is configured,
	// regardless of what Type field says. This implements "Postgres takes precedence silently".
	if ds.Spec.Database.PostgresSecretRef != nil {
		// Resolve PostgreSQL connection string from Secret
		connString, err := resolveSecretValue(ctx, c, ds.Namespace, *ds.Spec.Database.PostgresSecretRef)
		if err != nil {
			return cfg, fmt.Errorf("failed to resolve postgres secret: %w", err)
		}

		// PostgreSQL configured - set type AND connection string
		cfg.Type = "postgres"
		cfg.SQLite = nil
		cfg.Postgres = &connString
		return cfg, nil
	}

	// Postgres not configured - use SQLite settings
	if ds.Spec.Database.Type == "sqlite" || ds.Spec.Database.Type == "" {
		if ds.Spec.Database.SQLite != nil && ds.Spec.Database.SQLite.Path != "" {
			cfg.SQLite.Path = ds.Spec.Database.SQLite.Path
		}
	}

	return cfg, nil
}

// buildMetricsConfig constructs metrics configuration
func buildMetricsConfig(ds *dittoiov1alpha1.DittoServer) MetricsConfig {
	cfg := MetricsConfig{
		Enabled: false,
		Port:    DefaultMetricsPort,
	}

	if ds.Spec.Metrics == nil {
		return cfg
	}

	cfg.Enabled = ds.Spec.Metrics.Enabled
	if ds.Spec.Metrics.Port > 0 {
		cfg.Port = int(ds.Spec.Metrics.Port)
	}

	return cfg
}

// buildControlPlaneConfig constructs control plane API configuration
func buildControlPlaneConfig(ds *dittoiov1alpha1.DittoServer, jwtSecret string) ControlPlaneConfig {
	port := DefaultAPIPort
	if ds.Spec.ControlPlane != nil && ds.Spec.ControlPlane.Port > 0 {
		port = int(ds.Spec.ControlPlane.Port)
	}

	accessDuration := DefaultAccessDuration
	refreshDuration := DefaultRefreshDuration
	issuer := DefaultJWTIssuer

	if ds.Spec.Identity != nil && ds.Spec.Identity.JWT != nil {
		if ds.Spec.Identity.JWT.AccessTokenDuration != "" {
			accessDuration = ds.Spec.Identity.JWT.AccessTokenDuration
		}
		if ds.Spec.Identity.JWT.RefreshTokenDuration != "" {
			refreshDuration = ds.Spec.Identity.JWT.RefreshTokenDuration
		}
		if ds.Spec.Identity.JWT.Issuer != "" {
			issuer = ds.Spec.Identity.JWT.Issuer
		}
	}

	return ControlPlaneConfig{
		Port: port,
		JWT: JWTConfig{
			Secret:               jwtSecret,
			Issuer:               issuer,
			AccessTokenDuration:  accessDuration,
			RefreshTokenDuration: refreshDuration,
		},
	}
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

// resolveSecretValue resolves a single secret value from a Kubernetes Secret.
func resolveSecretValue(ctx context.Context, c client.Client, namespace string, secretRef corev1.SecretKeySelector) (string, error) {
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      secretRef.Name,
		Namespace: namespace,
	}

	if err := c.Get(ctx, secretKey, secret); err != nil {
		return "", fmt.Errorf("failed to get secret %s: %w", secretRef.Name, err)
	}

	if secretValue, ok := secret.Data[secretRef.Key]; ok {
		return string(secretValue), nil
	}

	return "", fmt.Errorf("key %s not found in secret %s", secretRef.Key, secretRef.Name)
}
