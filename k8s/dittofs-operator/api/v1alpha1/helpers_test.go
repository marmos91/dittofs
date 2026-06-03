/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newDittoServer(name, namespace string, cp *ControlPlaneAPIConfig) *DittoServer {
	return &DittoServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       DittoServerSpec{ControlPlane: cp},
	}
}

func TestGetAPIServiceURL(t *testing.T) {
	tests := []struct {
		name string
		cp   *ControlPlaneAPIConfig
		want string
	}{
		{
			name: "nil control plane defaults to http on default port",
			cp:   nil,
			want: "http://srv-api.ns.svc.cluster.local:8080",
		},
		{
			name: "tls disabled stays http",
			cp:   &ControlPlaneAPIConfig{TLS: false},
			want: "http://srv-api.ns.svc.cluster.local:8080",
		},
		{
			name: "tls enabled uses https so credentials are not plaintext",
			cp:   &ControlPlaneAPIConfig{TLS: true},
			want: "https://srv-api.ns.svc.cluster.local:8080",
		},
		{
			name: "custom port is honored",
			cp:   &ControlPlaneAPIConfig{Port: 9443},
			want: "http://srv-api.ns.svc.cluster.local:9443",
		},
		{
			name: "tls enabled with custom port",
			cp:   &ControlPlaneAPIConfig{Port: 9443, TLS: true},
			want: "https://srv-api.ns.svc.cluster.local:9443",
		},
		{
			name: "native cert secret upgrades scheme to https even without tls flag",
			cp:   &ControlPlaneAPIConfig{CertSecretName: "dfs-tls"},
			want: "https://srv-api.ns.svc.cluster.local:8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newDittoServer("srv", "ns", tt.cp).GetAPIServiceURL()
			if got != tt.want {
				t.Fatalf("GetAPIServiceURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGetAPIServiceURL_CredentialedPathNotPlaintextByOptIn asserts the
// credentialed operator->API path can be moved off plaintext via the spec knob
// without requiring any code change in the client.
func TestGetAPIServiceURL_TLSOptIn(t *testing.T) {
	ds := newDittoServer("srv", "ns", &ControlPlaneAPIConfig{TLS: true})
	if !strings.HasPrefix(ds.GetAPIServiceURL(), "https://") {
		t.Fatalf("expected https:// scheme when TLS is enabled, got %q", ds.GetAPIServiceURL())
	}
}

func TestNativeAndMutualTLSEnabled(t *testing.T) {
	cases := []struct {
		name       string
		cp         *ControlPlaneAPIConfig
		wantNative bool
		wantMutual bool
	}{
		{"nil control plane", nil, false, false},
		{"no cert secret", &ControlPlaneAPIConfig{}, false, false},
		{"cert secret only", &ControlPlaneAPIConfig{CertSecretName: "c"}, true, false},
		{"cert + client-ca", &ControlPlaneAPIConfig{CertSecretName: "c", ClientCASecretName: "ca"}, true, true},
		// A client-CA without a server cert is not honored (webhook rejects it).
		{"client-ca only", &ControlPlaneAPIConfig{ClientCASecretName: "ca"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := newDittoServer("srv", "ns", tc.cp)
			if got := ds.NativeTLSEnabled(); got != tc.wantNative {
				t.Errorf("NativeTLSEnabled() = %v, want %v", got, tc.wantNative)
			}
			if got := ds.MutualTLSEnabled(); got != tc.wantMutual {
				t.Errorf("MutualTLSEnabled() = %v, want %v", got, tc.wantMutual)
			}
		})
	}
}

func TestGetManagedJWTSecretName(t *testing.T) {
	ds := newDittoServer("srv", "ns", nil)
	if got, want := ds.GetManagedJWTSecretName(), "srv-jwt-secret"; got != want {
		t.Fatalf("GetManagedJWTSecretName() = %q, want %q", got, want)
	}
}

func TestGetEffectiveJWTSecretRef(t *testing.T) {
	t.Run("managed secret when user provides none", func(t *testing.T) {
		ds := newDittoServer("srv", "ns", nil)
		ref := ds.GetEffectiveJWTSecretRef()
		if ref.Name != ds.GetManagedJWTSecretName() {
			t.Fatalf("ref.Name = %q, want managed name %q", ref.Name, ds.GetManagedJWTSecretName())
		}
		if ref.Key != ManagedJWTSecretKey {
			t.Fatalf("ref.Key = %q, want %q", ref.Key, ManagedJWTSecretKey)
		}
	})

	t.Run("user-provided secret takes precedence", func(t *testing.T) {
		ds := newDittoServer("srv", "ns", nil)
		ds.Spec.Identity = &IdentityConfig{JWT: &JWTConfig{}}
		ds.Spec.Identity.JWT.SecretRef.Name = "my-secret"
		ds.Spec.Identity.JWT.SecretRef.Key = "my-key"
		if !ds.HasUserProvidedJWTSecret() {
			t.Fatal("HasUserProvidedJWTSecret() = false, want true")
		}
		ref := ds.GetEffectiveJWTSecretRef()
		if ref.Name != "my-secret" || ref.Key != "my-key" {
			t.Fatalf("GetEffectiveJWTSecretRef() = %+v, want user-provided secret", ref)
		}
	})
}
