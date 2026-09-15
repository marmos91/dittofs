package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// gssCtx returns a context carrying a negotiated RPCSEC_GSS service level, the
// way the GSS DATA dispatch attaches it to every request it processes.
func gssCtx(service uint32) context.Context {
	return gss.ContextWithSessionInfo(context.Background(), &gss.GSSSessionInfo{Service: service})
}

// TestCheckExportAccess_AuthFlavorPolicy covers the flavor gates and the GSS
// protection floor: which combinations of share policy and arriving credential
// the export accepts. No netgroup lookup is supplied, so only the flavor rules
// decide.
func TestCheckExportAccess_AuthFlavorPolicy(t *testing.T) {
	tests := []struct {
		name      string
		share     *runtime.Share
		ctx       context.Context
		flavor    uint32
		wantDeny  bool
		rationale string
	}{
		{
			name:      "AUTH_SYS accepted by a permissive share",
			share:     &runtime.Share{Name: "/export", AllowAuthSys: true},
			ctx:       context.Background(),
			flavor:    rpc.AuthUnix,
			rationale: "the default export policy accepts AUTH_SYS",
		},
		{
			name:      "AUTH_SYS refused when the share forbids it",
			share:     &runtime.Share{Name: "/export", AllowAuthSys: false},
			ctx:       context.Background(),
			flavor:    rpc.AuthUnix,
			wantDeny:  true,
			rationale: "AllowAuthSys=false is the whole point of the gate",
		},
		{
			name:      "AUTH_NONE refused when the share forbids AUTH_SYS",
			share:     &runtime.Share{Name: "/export", AllowAuthSys: false},
			ctx:       context.Background(),
			flavor:    rpc.AuthNull,
			wantDeny:  true,
			rationale: "a share refusing AUTH_SYS refuses everything weaker; no credential at all must not slip past",
		},
		{
			name:      "AUTH_SYS refused when the share requires Kerberos",
			share:     &runtime.Share{Name: "/export", AllowAuthSys: true, RequireKerberos: true},
			ctx:       context.Background(),
			flavor:    rpc.AuthUnix,
			wantDeny:  true,
			rationale: "RequireKerberos overrides AllowAuthSys",
		},
		{
			name:      "verified GSS accepted by a Kerberos-only share",
			share:     &runtime.Share{Name: "/export", AllowAuthSys: false, RequireKerberos: true},
			ctx:       gssCtx(gss.RPCGSSSvcNone),
			flavor:    rpc.AuthRPCSECGSS,
			rationale: "a processed GSS request satisfies both flavor gates when no floor is set",
		},
		{
			name:      "unverified GSS refused",
			share:     &runtime.Share{Name: "/export", AllowAuthSys: false, RequireKerberos: true},
			ctx:       context.Background(),
			flavor:    rpc.AuthRPCSECGSS,
			wantDeny:  true,
			rationale: "no session info means the credential was never processed as GSS; it must not stand in for Kerberos",
		},
		{
			name:      "krb5 refused below a krb5p floor",
			share:     &runtime.Share{Name: "/export", AllowAuthSys: true, MinKerberosLevel: models.KerberosLevelKrb5p},
			ctx:       gssCtx(gss.RPCGSSSvcNone),
			flavor:    rpc.AuthRPCSECGSS,
			wantDeny:  true,
			rationale: "authentication-only does not meet a privacy floor",
		},
		{
			name:      "krb5p accepted at a krb5p floor",
			share:     &runtime.Share{Name: "/export", AllowAuthSys: true, MinKerberosLevel: models.KerberosLevelKrb5p},
			ctx:       gssCtx(gss.RPCGSSSvcPrivacy),
			flavor:    rpc.AuthRPCSECGSS,
			rationale: "privacy meets the privacy floor",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckExportAccess(tc.ctx, tc.share, tc.flavor, nil, nil)
			if tc.wantDeny {
				if !errors.Is(err, ErrExportAccessDenied) {
					t.Fatalf("err = %v, want ErrExportAccessDenied (%s)", err, tc.rationale)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil (%s)", err, tc.rationale)
			}
		})
	}
}

// TestCheckExportAccess_Netgroup covers the client-address allowlist: it runs
// only when a lookup is supplied, and both an unparseable address and a failing
// lookup deny rather than fall through.
func TestCheckExportAccess_Netgroup(t *testing.T) {
	share := &runtime.Share{Name: "/export", AllowAuthSys: true}
	allow := func(context.Context, string, net.IP) (bool, error) { return true, nil }
	refuse := func(context.Context, string, net.IP) (bool, error) { return false, nil }
	fail := func(context.Context, string, net.IP) (bool, error) { return false, fmt.Errorf("netgroup store down") }

	if err := CheckExportAccess(context.Background(), share, rpc.AuthUnix, net.ParseIP("10.0.0.1"), allow); err != nil {
		t.Errorf("allowed client: err = %v, want nil", err)
	}
	if err := CheckExportAccess(context.Background(), share, rpc.AuthUnix, net.ParseIP("10.0.0.1"), refuse); !errors.Is(err, ErrExportAccessDenied) {
		t.Errorf("client outside the netgroup: err = %v, want ErrExportAccessDenied", err)
	}
	if err := CheckExportAccess(context.Background(), share, rpc.AuthUnix, net.ParseIP("10.0.0.1"), fail); !errors.Is(err, ErrExportAccessDenied) {
		t.Errorf("netgroup lookup failure must fail closed: err = %v, want ErrExportAccessDenied", err)
	}
	if err := CheckExportAccess(context.Background(), share, rpc.AuthUnix, nil, allow); !errors.Is(err, ErrExportAccessDenied) {
		t.Errorf("unparseable client address must fail closed: err = %v, want ErrExportAccessDenied", err)
	}
	// Without a lookup there is no address gate to apply, so an absent client
	// IP is not a denial on its own.
	if err := CheckExportAccess(context.Background(), share, rpc.AuthUnix, nil, nil); err != nil {
		t.Errorf("no netgroup lookup supplied: err = %v, want nil", err)
	}
}
