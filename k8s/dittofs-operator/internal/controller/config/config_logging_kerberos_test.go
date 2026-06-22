package config

import (
	"strings"
	"testing"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
)

// TestGenerateDittoFSConfig_Logging verifies the rendered logging: block tracks
// Spec.Logging and falls back to the operator defaults for unset fields.
func TestGenerateDittoFSConfig_Logging(t *testing.T) {
	t.Run("no logging spec -> defaults", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{}
		got := buildLoggingConfig(ds)
		if got.Level != DefaultLoggingLevel || got.Format != DefaultLoggingFormat || got.Output != DefaultLoggingOutput {
			t.Fatalf("expected defaults, got %+v", got)
		}
	})

	t.Run("level override is rendered", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				Logging: &dittoiov1alpha1.LoggingSpec{Level: "DEBUG"},
			},
		}
		yamlStr, err := GenerateDittoFSConfig(ds)
		if err != nil {
			t.Fatalf("GenerateDittoFSConfig: %v", err)
		}
		var parsed struct {
			Logging LoggingConfig `yaml:"logging"`
		}
		if err := yaml.Unmarshal([]byte(yamlStr), &parsed); err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		if parsed.Logging.Level != "DEBUG" {
			t.Fatalf("level = %q, want DEBUG\n%s", parsed.Logging.Level, yamlStr)
		}
		// Unset fields keep defaults.
		if parsed.Logging.Format != DefaultLoggingFormat || parsed.Logging.Output != DefaultLoggingOutput {
			t.Fatalf("unset fields lost defaults: %+v", parsed.Logging)
		}
	})

	t.Run("changing level changes rendered config (drives rollout)", func(t *testing.T) {
		mk := func(level string) string {
			ds := &dittoiov1alpha1.DittoServer{
				Spec: dittoiov1alpha1.DittoServerSpec{
					Logging: &dittoiov1alpha1.LoggingSpec{Level: level},
				},
			}
			out, err := GenerateDittoFSConfig(ds)
			if err != nil {
				t.Fatalf("GenerateDittoFSConfig: %v", err)
			}
			return out
		}
		if mk("INFO") == mk("DEBUG") {
			t.Fatal("rendered config identical across log levels; pod would not roll")
		}
	})
}

// TestGenerateDittoFSConfig_Kerberos verifies the rendered kerberos: block tracks
// Spec.Identity.Kerberos and never renders the keytab inline.
func TestGenerateDittoFSConfig_Kerberos(t *testing.T) {
	t.Run("disabled -> no kerberos block", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{}
		if k := buildKerberosConfig(ds); k != nil {
			t.Fatalf("expected nil kerberos config, got %+v", k)
		}
		yamlStr, err := GenerateDittoFSConfig(ds)
		if err != nil {
			t.Fatalf("GenerateDittoFSConfig: %v", err)
		}
		if strings.Contains(yamlStr, "kerberos:") {
			t.Fatalf("rendered config should omit kerberos block:\n%s", yamlStr)
		}
	})

	t.Run("enabled -> rendered block with mount paths, no keytab bytes", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				Identity: &dittoiov1alpha1.IdentityConfig{
					Kerberos: &dittoiov1alpha1.KerberosConfig{
						Enabled:          true,
						ServicePrincipal: "nfs/server.example.com@EXAMPLE.COM",
						Realm:            "EXAMPLE.COM",
						NetBIOSDomain:    "EXAMPLE",
						DNSDomain:        "example.com",
						KeytabSecretRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "krb-keytab"},
							Key:                  "dittofs.keytab",
						},
					},
				},
			},
		}
		yamlStr, err := GenerateDittoFSConfig(ds)
		if err != nil {
			t.Fatalf("GenerateDittoFSConfig: %v", err)
		}
		var parsed struct {
			Kerberos *KerberosConfig `yaml:"kerberos"`
		}
		if err := yaml.Unmarshal([]byte(yamlStr), &parsed); err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		if parsed.Kerberos == nil {
			t.Fatalf("expected kerberos block:\n%s", yamlStr)
		}
		if !parsed.Kerberos.Enabled ||
			parsed.Kerberos.ServicePrincipal != "nfs/server.example.com@EXAMPLE.COM" ||
			parsed.Kerberos.Realm != "EXAMPLE.COM" ||
			parsed.Kerberos.NetBIOSDomain != "EXAMPLE" ||
			parsed.Kerberos.DNSDomain != "example.com" {
			t.Fatalf("kerberos fields not rendered: %+v", parsed.Kerberos)
		}
		if parsed.Kerberos.KeytabPath != dittoiov1alpha1.KerberosKeytabFilePath() {
			t.Fatalf("keytab_path = %q, want %q", parsed.Kerberos.KeytabPath, dittoiov1alpha1.KerberosKeytabFilePath())
		}
		// The keytab key name may appear in the SecretRef, but the secret VALUE
		// must never be rendered into the YAML.
		if strings.Contains(yamlStr, "krb-keytab") {
			t.Fatalf("keytab Secret name leaked into config:\n%s", yamlStr)
		}
	})

	t.Run("empty keytab selector is treated as absent (no dangling keytab_path)", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				Identity: &dittoiov1alpha1.IdentityConfig{
					Kerberos: &dittoiov1alpha1.KerberosConfig{
						Enabled:          true,
						ServicePrincipal: "nfs/s@E.COM",
						// Empty selector ({}) — no Name. Must not set keytab_path.
						KeytabSecretRef:   &corev1.SecretKeySelector{},
						Krb5ConfSecretRef: &corev1.SecretKeySelector{},
					},
				},
			},
		}
		k := buildKerberosConfig(ds)
		if k == nil {
			t.Fatal("expected kerberos block (enabled)")
		}
		if k.KeytabPath != "" {
			t.Errorf("keytab_path = %q, want empty for selector without a Name", k.KeytabPath)
		}
		if k.Krb5Conf != "" {
			t.Errorf("krb5_conf = %q, want empty for selector without a Name", k.Krb5Conf)
		}
	})

	t.Run("krb5.conf secret ref wins over explicit path", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				Identity: &dittoiov1alpha1.IdentityConfig{
					Kerberos: &dittoiov1alpha1.KerberosConfig{
						Enabled:          true,
						ServicePrincipal: "nfs/s@E.COM",
						Krb5Conf:         "/etc/krb5.conf",
						KeytabSecretRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "kt"},
							Key:                  "k",
						},
						Krb5ConfSecretRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "krb5"},
							Key:                  "krb5.conf",
						},
					},
				},
			},
		}
		k := buildKerberosConfig(ds)
		if k.Krb5Conf != dittoiov1alpha1.KerberosKrb5ConfFilePath() {
			t.Fatalf("krb5_conf = %q, want mounted path %q", k.Krb5Conf, dittoiov1alpha1.KerberosKrb5ConfFilePath())
		}
	})
}
