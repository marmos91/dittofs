package auth

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ErrExportAccessDenied is returned by CheckExportAccess when a request fails
// the share's export access-control policy. Every denial wraps it, with the
// specific rule in the message, so a caller can map one decision onto its own
// protocol status (MNT3ERR_ACCES, NFS3ERR_ACCES, NFS4ERR_WRONGSEC) and log why.
var ErrExportAccessDenied = errors.New("export access denied")

// NetgroupChecker reports whether a client address is allowed by a share's
// netgroup allowlist. It is passed in rather than resolved from the Runtime so
// the policy above depends on the one lookup it needs, not on the whole
// control plane. *runtime.Runtime.CheckNetgroupAccess satisfies it.
type NetgroupChecker func(ctx context.Context, shareName string, clientIP net.IP) (bool, error)

// CheckExportAccess applies a share's export access-control policy to one
// request: which auth flavors the export accepts, the protection level a
// Kerberos session must have negotiated, and which client addresses may reach
// it. It returns nil when the request may proceed and an error wrapping
// ErrExportAccessDenied otherwise. share must not be nil.
//
// The rules, in order:
//
//   - AllowAuthSys=false refuses every non-GSS flavor, not AUTH_UNIX alone. A
//     share that refuses AUTH_SYS refuses everything weaker than it too, and
//     naming AUTH_UNIX alone would let an AUTH_NONE caller — no credential at
//     all — past the gate.
//   - RequireKerberos refuses every non-GSS flavor.
//   - min_kerberos_level is the GSS protection floor: a krb5i / krb5p export
//     rejects a Kerberos session negotiated at a weaker service level. The
//     negotiated level rides in ctx, attached by the GSS DATA dispatch to every
//     RPCSEC_GSS request it processes — control messages and failures are
//     answered there and never reach a caller of this function. So a claimed
//     GSS flavor with no session info was never processed as GSS, which happens
//     when no GSS processor is configured and the dispatch leaves flavor 6
//     unintercepted. That credential is unverified and cannot stand in for
//     Kerberos — RequireKerberos is satisfied by the flavor alone and
//     AllowAuthSys does not apply to it — so it is denied, not skipped.
//   - The netgroup allowlist gates the client address. It is checked only when
//     a netgroup lookup is supplied; a nil netgroup means the caller applies no
//     address gate here (an empty allowlist already means "allow all"). When
//     one is supplied, an unparseable client address and a failed lookup both
//     deny.
func CheckExportAccess(
	ctx context.Context,
	share *runtime.Share,
	authFlavor uint32,
	clientIP net.IP,
	netgroup NetgroupChecker,
) error {
	if !share.AllowAuthSys && authFlavor != rpc.AuthRPCSECGSS {
		return fmt.Errorf("%w: share %q accepts only Kerberos auth (flavor %d)",
			ErrExportAccessDenied, share.Name, authFlavor)
	}
	if share.RequireKerberos && authFlavor != rpc.AuthRPCSECGSS {
		return fmt.Errorf("%w: share %q requires Kerberos (flavor %d)",
			ErrExportAccessDenied, share.Name, authFlavor)
	}
	if authFlavor == rpc.AuthRPCSECGSS {
		si := gss.SessionInfoFromContext(ctx)
		if si == nil {
			return fmt.Errorf("%w: RPCSEC_GSS credential for share %q was not verified",
				ErrExportAccessDenied, share.Name)
		}
		if !MeetsMinKerberosLevel(share.MinKerberosLevel, si.Service) {
			return fmt.Errorf("%w: share %q requires min kerberos level %q (negotiated service %d)",
				ErrExportAccessDenied, share.Name, share.MinKerberosLevel, si.Service)
		}
	}

	if netgroup == nil {
		return nil
	}
	if clientIP == nil {
		return fmt.Errorf("%w: client address for share %q is not an IP, cannot evaluate the netgroup allowlist",
			ErrExportAccessDenied, share.Name)
	}
	allowed, err := netgroup(ctx, share.Name, clientIP)
	if err != nil {
		return fmt.Errorf("%w: netgroup check for share %q failed: %w",
			ErrExportAccessDenied, share.Name, err)
	}
	if !allowed {
		return fmt.Errorf("%w: client %s is not in the netgroup allowed for share %q",
			ErrExportAccessDenied, clientIP, share.Name)
	}
	return nil
}
