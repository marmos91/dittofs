package ldap

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// ldapAttrNameRe matches a syntactically valid LDAP attribute name
// (RFC 4512 descr: a leading letter followed by letters, digits, or hyphens).
// Used to reject a misconfigured user_attr before it produces an invalid filter.
var ldapAttrNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)

// IdmapMode selects how a resolved AD object's POSIX UID/GID is derived.
type IdmapMode string

const (
	// IdmapRFC2307 reads the uidNumber/gidNumber RFC2307 POSIX attributes
	// stamped on the AD objects (the idmap_ad model). This is authoritative
	// when the directory is provisioned with the RFC2307 schema.
	IdmapRFC2307 IdmapMode = "rfc2307"

	// IdmapRID derives UID/GID algorithmically from the object's RID (the last
	// sub-authority of its SID), matching Samba's idmap_rid. Used as a fallback
	// when RFC2307 attributes are absent. RID derivation is delegated to the
	// existing pkg/auth/sid mapper, read-only.
	IdmapRID IdmapMode = "rid"
)

// DefaultPort is the default LDAPS port. Plaintext (389) is never the default.
const (
	DefaultTimeout         = 10 * time.Second
	DefaultUserAttr        = "sAMAccountName"
	defaultGroupNameAttr   = "cn"
	DefaultMaxGroupResults = 200
)

// TLSConfig holds client-side TLS settings for the LDAP connection. DittoFS
// only consumes cert files; it is not a CA and does not issue certificates.
type TLSConfig struct {
	// CACertFile is a PEM bundle of CA certificates used to verify the LDAP
	// server certificate. When empty the system roots are used.
	CACertFile string `mapstructure:"ca_cert_file" yaml:"ca_cert_file,omitempty"`

	// ClientCertFile / ClientKeyFile enable mutual TLS (client certificate
	// auth). Both must be set together or neither.
	ClientCertFile string `mapstructure:"client_cert_file" yaml:"client_cert_file,omitempty"`
	ClientKeyFile  string `mapstructure:"client_key_file" yaml:"client_key_file,omitempty"`

	// InsecureSkipVerify disables server-certificate verification. This is a
	// foot-gun for production directories and is intended for lab use only.
	InsecureSkipVerify bool `mapstructure:"insecure_skip_verify" yaml:"insecure_skip_verify,omitempty"`

	// MinVersion is the minimum negotiated TLS version: "1.2" or "1.3".
	// Default (empty): "1.2".
	MinVersion string `mapstructure:"min_version" yaml:"min_version,omitempty"`
}

// Config configures the LDAP/AD identity provider.
//
// Security posture: the connection is encrypted by default. A plaintext (ldap://
// without StartTLS) connection is REFUSED unless AllowPlaintext is explicitly
// set — defense-oriented directories reject cleartext LDAP binds.
type Config struct {
	// Enabled turns the LDAP provider on. When false BuildIdentityResolver does
	// not register the provider.
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// URL is the directory URL, e.g. "ldaps://dc.example.com:636" (LDAPS) or
	// "ldap://dc.example.com:389" (plaintext or StartTLS). LDAPS is preferred.
	URL string `mapstructure:"url" yaml:"url"`

	// StartTLS upgrades a plaintext ldap:// connection to TLS via the StartTLS
	// extended operation. Ignored for ldaps:// (already encrypted). When the URL
	// is ldap:// and neither StartTLS nor AllowPlaintext is set, connecting is an
	// error.
	StartTLS bool `mapstructure:"start_tls" yaml:"start_tls"`

	// AllowPlaintext is the explicit opt-in required to send a bind over an
	// unencrypted ldap:// connection. Off by default.
	AllowPlaintext bool `mapstructure:"allow_plaintext" yaml:"allow_plaintext"`

	// BaseDN is the search base for user/group queries,
	// e.g. "DC=example,DC=com".
	BaseDN string `mapstructure:"base_dn" yaml:"base_dn"`

	// BindDN / BindPassword are the service-account credentials used to bind
	// before searching. Service-account bind is the minimum supported auth.
	BindDN       string `mapstructure:"bind_dn" yaml:"bind_dn"`
	BindPassword string `mapstructure:"bind_password" yaml:"bind_password,omitempty"`

	// UserAttr is the attribute matched against the credential's bare username
	// (default "sAMAccountName" for AD).
	UserAttr string `mapstructure:"user_attr" yaml:"user_attr,omitempty"`

	// Realm is the expected AD realm/domain (e.g. "EXAMPLE.COM"). Used to match
	// "user@REALM" principal credentials and stamped on the resolved Domain.
	Realm string `mapstructure:"realm" yaml:"realm"`

	// Idmap selects how UID/GID are derived: "rfc2307" (default) or "rid".
	Idmap IdmapMode `mapstructure:"idmap" yaml:"idmap"`

	// NestedGroups enables transitive (nested) group resolution. For AD this
	// uses the LDAP_MATCHING_RULE_IN_CHAIN matching rule on memberOf. When
	// false only direct group memberships are returned.
	NestedGroups bool `mapstructure:"nested_groups" yaml:"nested_groups"`

	// Timeout bounds each LDAP network operation (dial + bind + search).
	Timeout time.Duration `mapstructure:"timeout" yaml:"timeout"`

	// MaxGroupResults caps the number of (nested) groups resolved for a single
	// user. It bounds both the server-side result set of the IN_CHAIN search and
	// the per-group GID lookups that follow, so a deeply-nested principal cannot
	// trigger an unbounded fan-out of LDAP round-trips per authentication.
	// Defaults to DefaultMaxGroupResults.
	MaxGroupResults int `mapstructure:"max_group_results" yaml:"max_group_results,omitempty"`

	// TLS holds client TLS settings (CA, mTLS, min version).
	TLS TLSConfig `mapstructure:"tls" yaml:"tls"`
}

// redactedSecret is substituted for the bind password when the config is
// serialized for display.
const redactedSecret = "********"

// MarshalYAML redacts the bind password when the config is serialized for
// display (e.g. `dfs config show`). An empty password stays empty.
func (c Config) MarshalYAML() (interface{}, error) {
	type alias Config
	out := alias(c)
	if out.BindPassword != "" {
		out.BindPassword = redactedSecret
	}
	return out, nil
}

// MarshalJSON redacts the bind password on JSON serialization, mirroring
// MarshalYAML so a direct json.Marshal of the surrounding config (REST output,
// audit log, debug dump) can never leak the credential. An empty password
// stays empty.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias Config
	out := alias(c)
	if out.BindPassword != "" {
		out.BindPassword = redactedSecret
	}
	return json.Marshal(out)
}

// ApplyDefaults fills zero-valued fields with their defaults.
func (c *Config) ApplyDefaults() {
	if c.UserAttr == "" {
		c.UserAttr = DefaultUserAttr
	}
	if c.Idmap == "" {
		c.Idmap = IdmapRFC2307
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxGroupResults == 0 {
		c.MaxGroupResults = DefaultMaxGroupResults
	}
}

// IsLDAPS reports whether the URL uses the ldaps:// scheme (implicit TLS).
func (c Config) IsLDAPS() bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.URL)), "ldaps://")
}

// Validate checks the configuration for internal consistency. It is fail-fast
// and does not perform network I/O. Critically, it refuses a plaintext bind
// unless AllowPlaintext is explicitly set.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.URL) == "" {
		return fmt.Errorf("ldap.url is required when ldap is enabled")
	}
	scheme := strings.ToLower(strings.TrimSpace(c.URL))
	switch {
	case strings.HasPrefix(scheme, "ldaps://"):
		// Implicit TLS — always encrypted.
	case strings.HasPrefix(scheme, "ldap://"):
		// Plaintext scheme: must be upgraded via StartTLS or explicitly allowed.
		if !c.StartTLS && !c.AllowPlaintext {
			return fmt.Errorf("ldap.url uses plaintext ldap://; set ldap.start_tls=true to upgrade to TLS, or ldap.allow_plaintext=true to explicitly permit an unencrypted bind")
		}
	default:
		return fmt.Errorf("ldap.url must use the ldap:// or ldaps:// scheme (got %q)", c.URL)
	}
	if strings.TrimSpace(c.BaseDN) == "" {
		return fmt.Errorf("ldap.base_dn is required when ldap is enabled")
	}
	if strings.TrimSpace(c.BindDN) == "" {
		return fmt.Errorf("ldap.bind_dn is required (service-account bind is the minimum supported auth)")
	}
	if c.BindPassword == "" {
		// go-ldap sends an unauthenticated (anonymous) bind for an empty
		// password, which on many AD deployments silently succeeds with read
		// access — every lookup would then run as an unauthenticated session.
		return fmt.Errorf("ldap.bind_password is required (an empty password performs an anonymous bind)")
	}
	if c.UserAttr != "" && !ldapAttrNameRe.MatchString(c.UserAttr) {
		return fmt.Errorf("ldap.user_attr %q is not a valid LDAP attribute name", c.UserAttr)
	}
	if c.MaxGroupResults < 0 {
		return fmt.Errorf("ldap.max_group_results must be >= 0 (got %d)", c.MaxGroupResults)
	}
	switch c.Idmap {
	case "", IdmapRFC2307, IdmapRID:
	default:
		return fmt.Errorf("ldap.idmap %q is not supported (use %q or %q)", c.Idmap, IdmapRFC2307, IdmapRID)
	}
	if (c.TLS.ClientCertFile == "") != (c.TLS.ClientKeyFile == "") {
		return fmt.Errorf("ldap.tls.client_cert_file and ldap.tls.client_key_file must be set together")
	}
	if c.TLS.MinVersion != "" {
		if _, err := parseMinVersion(c.TLS.MinVersion); err != nil {
			return fmt.Errorf("ldap.tls.%w", err)
		}
	}
	if c.Timeout < 0 {
		return fmt.Errorf("ldap.timeout must be >= 0 (got %v)", c.Timeout)
	}
	return nil
}

// tlsClientConfig builds a *tls.Config for the LDAP client connection from the
// TLSConfig. The ServerName is supplied by the caller (derived from the URL
// host) so certificate hostname verification works.
func (c *Config) tlsClientConfig(serverName string) (*tls.Config, error) {
	minVersion := uint16(tls.VersionTLS12)
	if c.TLS.MinVersion != "" {
		v, err := parseMinVersion(c.TLS.MinVersion)
		if err != nil {
			return nil, err
		}
		minVersion = v
	}

	out := &tls.Config{
		ServerName:         serverName,
		MinVersion:         minVersion,
		InsecureSkipVerify: c.TLS.InsecureSkipVerify, //nolint:gosec // opt-in lab escape hatch, off by default
	}

	if c.TLS.CACertFile != "" {
		pem, err := os.ReadFile(c.TLS.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("read ldap CA cert file %s: %w", c.TLS.CACertFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ldap CA cert file %s contains no valid PEM certificate", c.TLS.CACertFile)
		}
		out.RootCAs = pool
	}

	if c.TLS.ClientCertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.TLS.ClientCertFile, c.TLS.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load ldap client key pair (cert=%s key=%s): %w", c.TLS.ClientCertFile, c.TLS.ClientKeyFile, err)
		}
		out.Certificates = []tls.Certificate{cert}
	}

	return out, nil
}

// parseMinVersion maps a min_version string ("1.2"/"1.3") to its crypto/tls
// constant. Mirrors internal/tlsconfig.ParseMinVersion but is local to avoid a
// server-side dependency on the client provider.
func parseMinVersion(s string) (uint16, error) {
	switch s {
	case "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("min_version %q is not supported (use \"1.2\" or \"1.3\")", s)
	}
}
