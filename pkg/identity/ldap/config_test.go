package ldap

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigValidate_PlaintextRefusedByDefault(t *testing.T) {
	cfg := &Config{
		Enabled: true,
		URL:     "ldap://dc.example.com:389",
		BaseDN:  "DC=example,DC=com",
		BindDN:  "CN=svc,DC=example,DC=com",
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected plaintext ldap:// without start_tls/allow_plaintext to be refused, got nil")
	}
}

func TestConfigValidate_PlaintextAllowedWithOptIn(t *testing.T) {
	cfg := &Config{
		Enabled:        true,
		URL:            "ldap://dc.example.com:389",
		BaseDN:         "DC=example,DC=com",
		BindDN:         "CN=svc,DC=example,DC=com",
		BindPassword:   "secret",
		AllowPlaintext: true,
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected allow_plaintext opt-in to pass validation, got %v", err)
	}
}

func TestConfigValidate_StartTLSPasses(t *testing.T) {
	cfg := &Config{
		Enabled:      true,
		URL:          "ldap://dc.example.com:389",
		BaseDN:       "DC=example,DC=com",
		BindDN:       "CN=svc,DC=example,DC=com",
		BindPassword: "secret",
		StartTLS:     true,
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected start_tls to pass validation, got %v", err)
	}
}

func TestConfigValidate_LDAPSPasses(t *testing.T) {
	cfg := &Config{
		Enabled:      true,
		URL:          "ldaps://dc.example.com:636",
		BaseDN:       "DC=example,DC=com",
		BindDN:       "CN=svc,DC=example,DC=com",
		BindPassword: "secret",
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected ldaps:// to pass validation, got %v", err)
	}
	if !cfg.IsLDAPS() {
		t.Error("IsLDAPS() should be true for ldaps://")
	}
}

func TestConfigValidate_DisabledSkips(t *testing.T) {
	cfg := &Config{Enabled: false}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled config must validate trivially, got %v", err)
	}
}

func TestConfigValidate_RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing url", Config{Enabled: true, BaseDN: "DC=x", BindDN: "CN=y", BindPassword: "p"}},
		{"missing base_dn", Config{Enabled: true, URL: "ldaps://x", BindDN: "CN=y", BindPassword: "p"}},
		{"missing bind_dn", Config{Enabled: true, URL: "ldaps://x", BaseDN: "DC=x", BindPassword: "p"}},
		{"missing bind_password", Config{Enabled: true, URL: "ldaps://x", BaseDN: "DC=x", BindDN: "CN=y"}},
		{"bad scheme", Config{Enabled: true, URL: "http://x", BaseDN: "DC=x", BindDN: "CN=y", BindPassword: "p"}},
		{"bad idmap", Config{Enabled: true, URL: "ldaps://x", BaseDN: "DC=x", BindDN: "CN=y", BindPassword: "p", Idmap: "bogus"}},
		{"bad user_attr", Config{Enabled: true, URL: "ldaps://x", BaseDN: "DC=x", BindDN: "CN=y", BindPassword: "p", UserAttr: "bad)attr"}},
		{"negative max_group_results", Config{Enabled: true, URL: "ldaps://x", BaseDN: "DC=x", BindDN: "CN=y", BindPassword: "p", MaxGroupResults: -1}},
		{"half mTLS", Config{Enabled: true, URL: "ldaps://x", BaseDN: "DC=x", BindDN: "CN=y", BindPassword: "p", TLS: TLSConfig{ClientCertFile: "a"}}},
		{"bad min version", Config{Enabled: true, URL: "ldaps://x", BaseDN: "DC=x", BindDN: "CN=y", BindPassword: "p", TLS: TLSConfig{MinVersion: "1.0"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cfg
			c.ApplyDefaults()
			if err := c.Validate(); err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()
	if cfg.UserAttr != DefaultUserAttr {
		t.Errorf("UserAttr default = %q, want %q", cfg.UserAttr, DefaultUserAttr)
	}
	if cfg.Idmap != IdmapRFC2307 {
		t.Errorf("Idmap default = %q, want %q", cfg.Idmap, IdmapRFC2307)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Timeout default = %v, want %v", cfg.Timeout, DefaultTimeout)
	}
}

func TestMarshalYAML_RedactsPassword(t *testing.T) {
	cfg := Config{BindPassword: "supersecret"}
	out, err := cfg.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), redactedSecret) {
		t.Errorf("bind password not redacted in YAML output: %s", data)
	}
	if strings.Contains(string(data), "supersecret") {
		t.Errorf("plaintext password leaked: %s", data)
	}
}

func TestMarshalJSON_RedactsPassword(t *testing.T) {
	cfg := Config{BindPassword: "supersecret"}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), redactedSecret) {
		t.Errorf("bind password not redacted in JSON output: %s", data)
	}
	if strings.Contains(string(data), "supersecret") {
		t.Errorf("plaintext password leaked in JSON: %s", data)
	}
}

func TestTLSClientConfig_MinVersion(t *testing.T) {
	cfg := &Config{TLS: TLSConfig{MinVersion: "1.3"}}
	tc, err := cfg.tlsClientConfig("host.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if tc.ServerName != "host.example.com" {
		t.Errorf("ServerName = %q", tc.ServerName)
	}
	if tc.MinVersion != 0x0304 { // tls.VersionTLS13
		t.Errorf("MinVersion = %x, want TLS1.3", tc.MinVersion)
	}
}
