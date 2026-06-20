package config

// DittoFSConfig represents the DittoFS configuration matching develop branch format.
// This is infrastructure-only config - stores, shares, adapters, users are managed via REST API.
type DittoFSConfig struct {
	Logging         LoggingConfig      `yaml:"logging"`
	ShutdownTimeout string             `yaml:"shutdown_timeout"`
	Database        DatabaseConfig     `yaml:"database"`
	ControlPlane    ControlPlaneConfig `yaml:"controlplane"`
	Admin           AdminConfig        `yaml:"admin,omitempty"`
	// Metrics renders the top-level metrics: block so the dfs server exposes the
	// Prometheus /metrics endpoint. Omitted entirely (nil pointer) unless the
	// CRD opts metrics in, preserving the disabled-by-default server behavior.
	Metrics *MetricsConfig `yaml:"metrics,omitempty"`
	// LDAP renders the top-level ldap: block for the LDAP/AD identity provider.
	// Omitted (nil pointer) unless the CRD configures LDAP. The bind password is
	// NOT rendered here — it is injected via the DITTOFS_LDAP_BIND_PASSWORD env
	// var sourced from a Kubernetes Secret.
	LDAP *LDAPConfig `yaml:"ldap,omitempty"`
}

// LDAPConfig mirrors the dfs server ldap: config keys (pkg/identity/ldap.Config).
// The bind password is intentionally absent; it is injected via env var from a
// Secret, never written to the ConfigMap.
type LDAPConfig struct {
	Enabled        bool           `yaml:"enabled"`
	URL            string         `yaml:"url"`
	StartTLS       bool           `yaml:"start_tls,omitempty"`
	AllowPlaintext bool           `yaml:"allow_plaintext,omitempty"`
	BaseDN         string         `yaml:"base_dn"`
	BindDN         string         `yaml:"bind_dn"`
	UserAttr       string         `yaml:"user_attr,omitempty"`
	Realm          string         `yaml:"realm,omitempty"`
	Idmap          string         `yaml:"idmap,omitempty"`
	NestedGroups   bool           `yaml:"nested_groups,omitempty"`
	TLS            *LDAPTLSConfig `yaml:"tls,omitempty"`
}

// LDAPTLSConfig mirrors the dfs server ldap.tls: keys.
type LDAPTLSConfig struct {
	CACertFile         string `yaml:"ca_cert_file,omitempty"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify,omitempty"`
}

// MetricsConfig mirrors the dfs server metrics: config keys (pkg/config).
// Bind host is always 0.0.0.0 in-cluster: the server's 127.0.0.1 default is
// unreachable from the metrics Service. Network isolation is provided by the
// metrics Service scope and NetworkPolicy, not by loopback binding.
type MetricsConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Host      string `yaml:"host"`
	Port      int    `yaml:"port"`
	Path      string `yaml:"path"`
	Auth      string `yaml:"auth,omitempty"`
	TokenFile string `yaml:"token_file,omitempty"`
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
	// Host binds the API server. The server defaults to 127.0.0.1 (loopback
	// only), which is unreachable from other pods; in-cluster the operator
	// always renders 0.0.0.0 so the API Service can route to it. Edge TLS is
	// terminated by the Service/ingress/mesh.
	Host string    `yaml:"host"`
	Port int       `yaml:"port"`
	JWT  JWTConfig `yaml:"jwt"`
	// TLS renders controlplane.tls.* so the pod serves native (in-pod) HTTPS.
	// Omitted entirely (omitempty on the pointer) unless a cert Secret was
	// named, preserving the plain-http / edge-terminated default.
	TLS *TLSConfig `yaml:"tls,omitempty"`
}

// TLSConfig points the server at the cert/key (and optional client-CA) files
// mounted from the operator-provided Secret(s). It mirrors the dfs
// controlplane.tls config keys.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	ClientCA string `yaml:"client_ca,omitempty"`
}

// JWTConfig configures JWT authentication.
// Note: the server hardcodes the JWT issuer ("dittofs") and exposes no issuer
// config key, so this type deliberately omits Issuer to avoid emitting a key
// the server silently discards.
type JWTConfig struct {
	AccessTokenDuration  string `yaml:"access_token_duration"`
	RefreshTokenDuration string `yaml:"refresh_token_duration"`
}

// AdminConfig configures the initial admin user
type AdminConfig struct {
	Username string `yaml:"username"`
	Email    string `yaml:"email,omitempty"`
}
