package config

import (
	"strings"
	"testing"

	dittoiov1alpha1 "github.com/marmos91/dittofs/k8s/dittofs-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

// TestGenerateDittoFSConfig_NativeTLS verifies the rendered controlplane.tls.*
// block tracks the CertSecretName / ClientCASecretName CRD fields.
func TestGenerateDittoFSConfig_NativeTLS(t *testing.T) {
	t.Run("no cert secret -> no tls block", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				ControlPlane: &dittoiov1alpha1.ControlPlaneAPIConfig{},
			},
		}
		cp := buildControlPlaneConfig(ds)
		if cp.TLS != nil {
			t.Fatalf("expected no TLS block without a cert secret, got %+v", cp.TLS)
		}
		yamlStr, err := GenerateDittoFSConfig(ds)
		if err != nil {
			t.Fatalf("GenerateDittoFSConfig: %v", err)
		}
		if strings.Contains(yamlStr, "tls:") {
			t.Fatalf("rendered config should omit the tls block:\n%s", yamlStr)
		}
	})

	t.Run("cert secret -> cert_file + key_file, no client_ca", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				ControlPlane: &dittoiov1alpha1.ControlPlaneAPIConfig{
					CertSecretName: "dfs-tls",
				},
			},
		}
		cp := buildControlPlaneConfig(ds)
		if cp.TLS == nil {
			t.Fatal("expected a TLS block when a cert secret is named")
		}
		if cp.TLS.CertFile != dittoiov1alpha1.TLSCertFilePath() {
			t.Errorf("cert_file = %q, want %q", cp.TLS.CertFile, dittoiov1alpha1.TLSCertFilePath())
		}
		if cp.TLS.KeyFile != dittoiov1alpha1.TLSKeyFilePath() {
			t.Errorf("key_file = %q, want %q", cp.TLS.KeyFile, dittoiov1alpha1.TLSKeyFilePath())
		}
		if cp.TLS.ClientCA != "" {
			t.Errorf("client_ca should be empty without a client-CA secret, got %q", cp.TLS.ClientCA)
		}
		// The cert/key files live under the mount path; the rendered YAML must
		// reference them so the server loads them.
		assertRenderedTLS(t, ds, dittoiov1alpha1.TLSCertFilePath(), dittoiov1alpha1.TLSKeyFilePath())
	})

	t.Run("cert + client-CA secret -> client_ca rendered", func(t *testing.T) {
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				ControlPlane: &dittoiov1alpha1.ControlPlaneAPIConfig{
					CertSecretName:     "dfs-tls",
					ClientCASecretName: "dfs-client-ca",
				},
			},
		}
		cp := buildControlPlaneConfig(ds)
		if cp.TLS == nil || cp.TLS.ClientCA != dittoiov1alpha1.TLSClientCAFilePath() {
			t.Fatalf("client_ca = %q, want %q", cp.TLS.ClientCA, dittoiov1alpha1.TLSClientCAFilePath())
		}
	})

	t.Run("client-CA without cert secret -> no tls block (guarded by webhook)", func(t *testing.T) {
		// NativeTLSEnabled keys off the cert secret only, so a stray
		// ClientCASecretName alone renders nothing (the webhook rejects it).
		ds := &dittoiov1alpha1.DittoServer{
			Spec: dittoiov1alpha1.DittoServerSpec{
				ControlPlane: &dittoiov1alpha1.ControlPlaneAPIConfig{
					ClientCASecretName: "dfs-client-ca",
				},
			},
		}
		if cp := buildControlPlaneConfig(ds); cp.TLS != nil {
			t.Fatalf("expected no TLS block without a cert secret, got %+v", cp.TLS)
		}
	})
}

func assertRenderedTLS(t *testing.T, ds *dittoiov1alpha1.DittoServer, certPath, keyPath string) {
	t.Helper()
	yamlStr, err := GenerateDittoFSConfig(ds)
	if err != nil {
		t.Fatalf("GenerateDittoFSConfig: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cp, _ := doc["controlplane"].(map[string]any)
	tlsBlock, ok := cp["tls"].(map[string]any)
	if !ok {
		t.Fatalf("rendered controlplane has no tls block:\n%s", yamlStr)
	}
	if tlsBlock["cert_file"] != certPath {
		t.Errorf("rendered cert_file = %v, want %q", tlsBlock["cert_file"], certPath)
	}
	if tlsBlock["key_file"] != keyPath {
		t.Errorf("rendered key_file = %v, want %q", tlsBlock["key_file"], keyPath)
	}
}
