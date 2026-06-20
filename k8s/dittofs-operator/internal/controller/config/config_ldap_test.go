package config

import (
	"strings"
	"testing"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

// TestGenerateDittoFSConfig_LDAP verifies the rendered ldap: block tracks the
// Spec.Identity.LDAP CRD fields and never leaks the bind password.
func TestGenerateDittoFSConfig_LDAP(t *testing.T) {
	t.Run("no ldap spec -> no ldap block", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{}
		if l := buildLDAPConfig(ds); l != nil {
			t.Fatalf("expected nil LDAP config, got %+v", l)
		}
		yamlStr, err := GenerateDittoFSConfig(ds)
		if err != nil {
			t.Fatalf("GenerateDittoFSConfig: %v", err)
		}
		if strings.Contains(yamlStr, "ldap:") {
			t.Fatalf("rendered config should omit the ldap block:\n%s", yamlStr)
		}
	})

	t.Run("ldap spec -> rendered ldap block, password never rendered", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				Identity: &dittoiov1alpha1.IdentityConfig{
					LDAP: &dittoiov1alpha1.LDAPConfig{
						Enabled:      true,
						URL:          "ldaps://dc.example.com:636",
						BaseDN:       "DC=example,DC=com",
						BindDN:       "CN=svc,DC=example,DC=com",
						Realm:        "EXAMPLE.COM",
						Idmap:        "rfc2307",
						NestedGroups: true,
						CACertFile:   "/etc/dittofs/ad-ca.pem",
					},
				},
			},
		}

		yamlStr, err := GenerateDittoFSConfig(ds)
		if err != nil {
			t.Fatalf("GenerateDittoFSConfig: %v", err)
		}

		// Re-parse to assert the rendered keys.
		var parsed struct {
			LDAP *LDAPConfig `yaml:"ldap"`
		}
		if err := yaml.Unmarshal([]byte(yamlStr), &parsed); err != nil {
			t.Fatalf("re-parse rendered config: %v", err)
		}
		if parsed.LDAP == nil {
			t.Fatalf("expected ldap block in:\n%s", yamlStr)
		}
		if !parsed.LDAP.Enabled || parsed.LDAP.URL != "ldaps://dc.example.com:636" {
			t.Errorf("unexpected ldap fields: %+v", parsed.LDAP)
		}
		if parsed.LDAP.TLS == nil || parsed.LDAP.TLS.CACertFile != "/etc/dittofs/ad-ca.pem" {
			t.Errorf("expected ldap.tls.ca_cert_file rendered, got %+v", parsed.LDAP.TLS)
		}

		// The bind password must never appear in the ConfigMap YAML.
		if strings.Contains(strings.ToLower(yamlStr), "bind_password") {
			t.Errorf("bind_password key must not be rendered:\n%s", yamlStr)
		}
	})
}
