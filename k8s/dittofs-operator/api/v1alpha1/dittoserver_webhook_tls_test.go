package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestValidateDittoServer_TLS covers the native-TLS validation rules:
//   - a client-CA Secret without a server-cert Secret is rejected (mTLS needs a
//     server certificate);
//   - a server-cert Secret with tls=false emits a heads-up warning (the
//     operator will still dial https).
func TestValidateDittoServer_TLS(t *testing.T) {
	mk := func(cp *ControlPlaneAPIConfig) *DittoServer {
		return &DittoServer{
			ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: "default"},
			Spec: DittoServerSpec{
				// Storage is required earlier in validation; provide it so the
				// TLS rules under test are actually reached.
				Storage:      StorageSpec{MetadataSize: "1Gi", CacheSize: "1Gi"},
				ControlPlane: cp,
			},
		}
	}

	t.Run("client-ca without cert secret is rejected", func(t *testing.T) {
		_, err := mk(&ControlPlaneAPIConfig{ClientCASecretName: "ca"}).validateDittoServer()
		if err == nil || !strings.Contains(err.Error(), "clientCASecretName requires") {
			t.Fatalf("expected clientCASecretName-requires-certSecretName error, got %v", err)
		}
	})

	t.Run("cert secret with tls=false warns", func(t *testing.T) {
		warnings, err := mk(&ControlPlaneAPIConfig{CertSecretName: "tls", TLS: false}).validateDittoServer()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		found := false
		for _, w := range warnings {
			if strings.Contains(w, "serves native TLS") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a native-TLS heads-up warning, got %v", warnings)
		}
	})

	t.Run("cert + client-ca + tls=true is clean", func(t *testing.T) {
		warnings, err := mk(&ControlPlaneAPIConfig{CertSecretName: "tls", ClientCASecretName: "ca", TLS: true}).validateDittoServer()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, w := range warnings {
			if strings.Contains(w, "serves native TLS") {
				t.Fatalf("did not expect the tls=false warning when tls=true: %v", warnings)
			}
		}
	})
}
