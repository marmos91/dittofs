package auth

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestMeetsMinKerberosLevel(t *testing.T) {
	tests := []struct {
		name       string
		minLevel   string
		negotiated uint32
		want       bool
	}{
		// No floor: any service level (including a non-GSS 0) is allowed.
		{"empty floor / auth-only", "", gss.RPCGSSSvcNone, true},
		{"empty floor / privacy", "", gss.RPCGSSSvcPrivacy, true},
		{"unknown level treated as no floor", "krb5x", gss.RPCGSSSvcNone, true},

		// krb5 floor = authentication only: every GSS service satisfies it.
		{"krb5 floor / auth-only", models.KerberosLevelKrb5, gss.RPCGSSSvcNone, true},
		{"krb5 floor / integrity", models.KerberosLevelKrb5, gss.RPCGSSSvcIntegrity, true},
		{"krb5 floor / privacy", models.KerberosLevelKrb5, gss.RPCGSSSvcPrivacy, true},

		// krb5i floor = integrity: auth-only is rejected, integrity/privacy pass.
		{"krb5i floor / auth-only rejected", models.KerberosLevelKrb5i, gss.RPCGSSSvcNone, false},
		{"krb5i floor / integrity", models.KerberosLevelKrb5i, gss.RPCGSSSvcIntegrity, true},
		{"krb5i floor / privacy", models.KerberosLevelKrb5i, gss.RPCGSSSvcPrivacy, true},

		// krb5p floor = privacy: only privacy passes.
		{"krb5p floor / auth-only rejected", models.KerberosLevelKrb5p, gss.RPCGSSSvcNone, false},
		{"krb5p floor / integrity rejected", models.KerberosLevelKrb5p, gss.RPCGSSSvcIntegrity, false},
		{"krb5p floor / privacy", models.KerberosLevelKrb5p, gss.RPCGSSSvcPrivacy, true},

		// Fail-closed: a floor above krb5 with a missing/zero service level
		// (no GSS session info) is rejected.
		{"krb5i floor / zero service fails closed", models.KerberosLevelKrb5i, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MeetsMinKerberosLevel(tc.minLevel, tc.negotiated); got != tc.want {
				t.Fatalf("MeetsMinKerberosLevel(%q, %d) = %v, want %v",
					tc.minLevel, tc.negotiated, got, tc.want)
			}
		})
	}
}
