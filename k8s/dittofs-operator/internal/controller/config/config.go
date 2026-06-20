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
	// DefaultShutdownTimeoutSeconds is the numeric form of DefaultShutdownTimeout.
	// Keep this in sync with DefaultShutdownTimeout; the controller derives the pod's
	// TerminationGracePeriodSeconds from it so the grace period and the server's
	// per-stage shutdown budget stay coupled.
	DefaultShutdownTimeoutSeconds = 30
	// DefaultSQLitePath lives UNDER the dedicated control-plane PVC (mounted at
	// /data/controlplane) so the control-plane DB — which holds the metadata-store
	// registry and share definitions — survives pod restart/reschedule. A path on
	// the ephemeral container overlay is wiped on every restart, silently orphaning
	// all on-disk data.
	DefaultSQLitePath      = "/data/controlplane/controlplane.db"
	DefaultAPIPort         = 8080
	DefaultAccessDuration  = "15m"
	DefaultRefreshDuration = "168h" // 7 days
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
		Metrics:         buildMetricsConfig(dittoServer),
		LDAP:            buildLDAPConfig(dittoServer),
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

	cp := ControlPlaneConfig{
		// Bind all interfaces: the server's 127.0.0.1 default is unreachable
		// from the API Service, so in-cluster we always listen on 0.0.0.0.
		Host: "0.0.0.0",
		Port: port,
		JWT: JWTConfig{
			AccessTokenDuration:  stringOrDefault(jwtCfg.AccessTokenDuration, DefaultAccessDuration),
			RefreshTokenDuration: stringOrDefault(jwtCfg.RefreshTokenDuration, DefaultRefreshDuration),
		},
	}

	// Native TLS: when a server-certificate Secret is named, point the server
	// at the mounted cert/key (and optional client-CA) so the pod serves HTTPS
	// end-to-end. The controller mounts the matching Secret(s) at these paths.
	if ds.NativeTLSEnabled() {
		tls := &TLSConfig{
			CertFile: dittoiov1alpha1.TLSCertFilePath(),
			KeyFile:  dittoiov1alpha1.TLSKeyFilePath(),
		}
		if ds.MutualTLSEnabled() {
			tls.ClientCA = dittoiov1alpha1.TLSClientCAFilePath()
		}
		cp.TLS = tls
	}

	return cp
}

// buildMetricsConfig renders the metrics: block when the CRD opts metrics in.
// Returns nil (no metrics: key) when disabled, preserving the server's
// disabled-by-default behavior. The endpoint always binds 0.0.0.0 in-cluster
// (the metrics Service cannot reach a loopback-bound listener); isolation is
// provided by the dedicated metrics Service + NetworkPolicy. When a bearer
// token Secret is referenced, auth=token and the token file path points at the
// mounted Secret.
func buildMetricsConfig(ds *dittoiov1alpha1.DittoServer) *MetricsConfig {
	if !ds.MetricsEnabled() {
		return nil
	}

	cfg := &MetricsConfig{
		Enabled: true,
		Host:    "0.0.0.0",
		Port:    int(ds.MetricsPort()),
		Path:    ds.MetricsPath(),
		Auth:    "none",
	}

	if ds.MetricsBearerTokenSecret() != nil {
		cfg.Auth = "token"
		cfg.TokenFile = dittoiov1alpha1.MetricsTokenFilePath()
	}

	return cfg
}

// buildLDAPConfig renders the ldap: block when the CRD configures the LDAP/AD
// identity provider. Returns nil (no ldap: key) when LDAP is absent, preserving
// the disabled-by-default server behavior. The bind password is never rendered
// here; it is injected via the DITTOFS_LDAP_BIND_PASSWORD env var from a Secret.
func buildLDAPConfig(ds *dittoiov1alpha1.DittoServer) *LDAPConfig {
	if ds.Spec.Identity == nil || ds.Spec.Identity.LDAP == nil {
		return nil
	}
	l := ds.Spec.Identity.LDAP

	cfg := &LDAPConfig{
		Enabled:        l.Enabled,
		URL:            l.URL,
		StartTLS:       l.StartTLS,
		AllowPlaintext: l.AllowPlaintext,
		BaseDN:         l.BaseDN,
		BindDN:         l.BindDN,
		UserAttr:       l.UserAttr,
		Realm:          l.Realm,
		Idmap:          l.Idmap,
		NestedGroups:   l.NestedGroups,
	}

	if l.CACertFile != "" || l.InsecureSkipVerify {
		cfg.TLS = &LDAPTLSConfig{
			CACertFile:         l.CACertFile,
			InsecureSkipVerify: l.InsecureSkipVerify,
		}
	}

	return cfg
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
