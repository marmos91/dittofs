package apiclient

import "fmt"

// IdentityProviderSummary reports the configured/enabled state of an identity
// provider type as returned by ListIdentityProviders.
type IdentityProviderSummary struct {
	Type       string `json:"type"`
	Enabled    bool   `json:"enabled"`
	Configured bool   `json:"configured"`
}

// LDAPTLSConfig is the TLS sub-config of an LDAP identity provider.
type LDAPTLSConfig struct {
	CACertFile         string `json:"ca_cert_file"`
	ClientCertFile     string `json:"client_cert_file"`
	ClientKeyFile      string `json:"client_key_file"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	MinVersion         string `json:"min_version"`
}

// LDAPProviderConfig mirrors the server's LDAP identity-provider config. The
// bind password is write-only: it is accepted on Put/Test and returned as
// "********" (or "") on Get — submitting the placeholder back preserves the
// stored secret.
type LDAPProviderConfig struct {
	Enabled         bool          `json:"enabled"`
	URL             string        `json:"url"`
	StartTLS        bool          `json:"start_tls"`
	AllowPlaintext  bool          `json:"allow_plaintext"`
	BaseDN          string        `json:"base_dn"`
	BindDN          string        `json:"bind_dn"`
	BindPassword    string        `json:"bind_password,omitempty"`
	UserAttr        string        `json:"user_attr"`
	Realm           string        `json:"realm"`
	Idmap           string        `json:"idmap"`
	NestedGroups    bool          `json:"nested_groups"`
	Timeout         string        `json:"timeout"`
	MaxGroupResults int           `json:"max_group_results"`
	TLS             LDAPTLSConfig `json:"tls"`
}

// KerberosProviderConfig mirrors the server's Kerberos identity-provider config.
// A change takes effect on the next server restart.
type KerberosProviderConfig struct {
	Enabled          bool   `json:"enabled"`
	KeytabPath       string `json:"keytab_path"`
	ServicePrincipal string `json:"service_principal"`
	Realm            string `json:"realm"`
	NetBIOSDomain    string `json:"netbios_domain"`
	DNSDomain        string `json:"dns_domain"`
	Krb5Conf         string `json:"krb5_conf"`
	MaxClockSkew     string `json:"max_clock_skew"`
	ContextTTL       string `json:"context_ttl"`
	MaxContexts      int    `json:"max_contexts"`
}

// IdentityProviderTestResult is the result of a non-persisting provider test.
type IdentityProviderTestResult struct {
	OK        bool   `json:"ok"`
	Stage     string `json:"stage,omitempty"`
	Message   string `json:"message,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

// ListIdentityProviders returns the configured/enabled state of each identity
// provider type (no secrets).
func (c *Client) ListIdentityProviders() ([]IdentityProviderSummary, error) {
	var out []IdentityProviderSummary
	if err := c.get("/api/v1/identity-providers", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetLDAPConfig returns the current LDAP provider config (bind password redacted).
func (c *Client) GetLDAPConfig() (*LDAPProviderConfig, error) {
	return getIdentityProviderConfig[LDAPProviderConfig](c, "ldap")
}

// PutLDAPConfig creates or replaces the LDAP provider config and hot-reloads it.
func (c *Client) PutLDAPConfig(cfg *LDAPProviderConfig) (*LDAPProviderConfig, error) {
	return putIdentityProviderConfig(c, "ldap", cfg)
}

// TestLDAPConfig dials+binds using cfg without persisting it.
func (c *Client) TestLDAPConfig(cfg *LDAPProviderConfig) (*IdentityProviderTestResult, error) {
	return testIdentityProvider(c, "ldap", cfg)
}

// GetKerberosConfig returns the current Kerberos provider config.
func (c *Client) GetKerberosConfig() (*KerberosProviderConfig, error) {
	return getIdentityProviderConfig[KerberosProviderConfig](c, "kerberos")
}

// PutKerberosConfig creates or replaces the Kerberos provider config (applied on
// the next server restart).
func (c *Client) PutKerberosConfig(cfg *KerberosProviderConfig) (*KerberosProviderConfig, error) {
	return putIdentityProviderConfig(c, "kerberos", cfg)
}

// TestKerberosConfig validates cfg and loads its keytab without persisting.
func (c *Client) TestKerberosConfig(cfg *KerberosProviderConfig) (*IdentityProviderTestResult, error) {
	return testIdentityProvider(c, "kerberos", cfg)
}

func getIdentityProviderConfig[T any](c *Client, providerType string) (*T, error) {
	var out T
	if err := c.get(fmt.Sprintf("/api/v1/identity-providers/%s/config", providerType), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func putIdentityProviderConfig[T any](c *Client, providerType string, cfg *T) (*T, error) {
	var out T
	if err := c.put(fmt.Sprintf("/api/v1/identity-providers/%s/config", providerType), cfg, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func testIdentityProvider[T any](c *Client, providerType string, cfg *T) (*IdentityProviderTestResult, error) {
	var out IdentityProviderTestResult
	if err := c.post(fmt.Sprintf("/api/v1/identity-providers/%s/test", providerType), cfg, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
