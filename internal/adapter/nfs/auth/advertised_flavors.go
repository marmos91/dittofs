package auth

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// krb5Pseudoflavors pairs each Kerberos pseudoflavor (RFC 2623 §2.1) with the
// RPCSEC_GSS service level it negotiates, so a share's protection floor can be
// applied to the advertised set with the same comparison that enforces it on an
// arriving request.
var krb5Pseudoflavors = []struct {
	pseudoflavor uint32
	service      uint32
}{
	{gss.PseudoFlavorKrb5, gss.RPCGSSSvcNone},
	{gss.PseudoFlavorKrb5i, gss.RPCGSSSvcIntegrity},
	{gss.PseudoFlavorKrb5p, gss.RPCGSSSvcPrivacy},
}

// AdvertisedAuthFlavors returns the auth flavors a share will actually accept,
// for the MNT reply's fhstatus3.auth_flavors list (RFC 1813 App I).
//
// The list tells a client which flavors this export accepts, and mount.nfs and
// automounters negotiate `sec=` from it. Advertising a flavor the share then
// refuses sends a client to a mount that cannot succeed, so this must agree with
// the gates applied to an arriving mount:
//
//   - AUTH_UNIX is offered only when the share allows AUTH_SYS and does not
//     mandate Kerberos.
//   - Each Kerberos pseudoflavor is offered only when it meets the share's
//     protection floor — a krb5p export does not advertise krb5 or krb5i.
//
// Kerberos flavors are offered only when the server has Kerberos configured at
// all; without a processor no GSS context can be established, so advertising
// them would point a client at a flavor that cannot work.
//
// The result can be empty, which is a faithful answer: a share that forbids
// AUTH_SYS on a server with no Kerberos accepts nothing.
func AdvertisedAuthFlavors(share *runtime.Share, kerberosEnabled bool) []int32 {
	var flavors []int32

	if share.AllowAuthSys && !share.RequireKerberos {
		flavors = append(flavors, int32(rpc.AuthUnix))
	}

	if kerberosEnabled {
		for _, f := range krb5Pseudoflavors {
			if MeetsMinKerberosLevel(share.MinKerberosLevel, f.service) {
				flavors = append(flavors, int32(f.pseudoflavor))
			}
		}
	}

	return flavors
}
