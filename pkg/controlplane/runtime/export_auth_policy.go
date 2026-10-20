package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ErrExportAccessDenied is returned by CheckShareAccess when a request fails a
// share's access policy. Every denial wraps it, with the rule that fired in the
// message, so each protocol can map the one decision onto its own status and
// log why.
var ErrExportAccessDenied = errors.New("export access denied")

// KerberosProtection is the protection a request's Kerberos context gives it,
// in a form both protocols can state: NFS derives it from the RPCSEC_GSS
// service a request negotiated, SMB from how the session authenticated and
// whether its traffic on the tree is signed or encrypted. The values are
// ordered, so a floor is a >= comparison.
type KerberosProtection uint8

const (
	// NoKerberos is a request that was not authenticated with Kerberos.
	NoKerberos KerberosProtection = iota
	// KerberosAuthOnly is Kerberos authentication with no per-message protection (krb5).
	KerberosAuthOnly
	// KerberosIntegrity adds per-message integrity (krb5i).
	KerberosIntegrity
	// KerberosPrivacy adds per-message confidentiality (krb5p).
	KerberosPrivacy
)

// RequiredKerberosProtection maps a share's min_kerberos_level to the
// protection a Kerberos request must have. An empty or unrecognised level sets
// no floor and maps to NoKerberos, which every request meets.
func RequiredKerberosProtection(minLevel string) KerberosProtection {
	switch minLevel {
	case models.KerberosLevelKrb5:
		return KerberosAuthOnly
	case models.KerberosLevelKrb5i:
		return KerberosIntegrity
	case models.KerberosLevelKrb5p:
		return KerberosPrivacy
	default:
		return NoKerberos
	}
}

// ShareRequiresKerberos reports whether a share admits only Kerberos-
// authenticated requests: require_kerberos is set, or allow_auth_sys is off.
// Both refuse every request weaker than Kerberos — on NFS, AUTH_NONE as well as
// AUTH_SYS — so the two are one rule here.
func ShareRequiresKerberos(share *Share) bool {
	return share.RequireKerberos || !share.AllowAuthSys
}

// NetgroupChecker reports whether a client address is allowed by a share's
// netgroup allowlist. *Runtime.CheckNetgroupAccess satisfies it.
type NetgroupChecker func(ctx context.Context, shareName string, clientIP net.IP) (bool, error)

// CheckShareAccess applies the protocol-neutral part of a share's access
// policy to one request: whether the share admits a request with this Kerberos
// protection, and whether the client address is in the share's netgroup
// allowlist. It returns nil when the request may proceed and an error wrapping
// ErrExportAccessDenied otherwise. share must not be nil.
//
// The protection floor (min_kerberos_level) applies to Kerberos requests only.
// A request without Kerberos is governed by ShareRequiresKerberos alone, so a
// share that sets a floor but still admits weaker authentication admits it.
//
// The netgroup allowlist gates the client address, and only when a lookup is
// supplied: a nil netgroup means the caller applies no address gate here (an
// empty allowlist already means "allow all"). When one is supplied, an
// unparseable client address and a failed lookup both deny.
func CheckShareAccess(
	ctx context.Context,
	share *Share,
	protection KerberosProtection,
	clientIP net.IP,
	netgroup NetgroupChecker,
) error {
	if protection == NoKerberos {
		if ShareRequiresKerberos(share) {
			return fmt.Errorf("%w: share %q accepts only Kerberos authentication",
				ErrExportAccessDenied, share.Name)
		}
	} else if protection < RequiredKerberosProtection(share.MinKerberosLevel) {
		return fmt.Errorf("%w: share %q requires min kerberos level %q (request has protection %d)",
			ErrExportAccessDenied, share.Name, share.MinKerberosLevel, protection)
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

// ExportAcceptsNoAuthFlavor reports whether a share's NFS export policy leaves
// no auth flavor a client could ever use.
//
// The flavor set an export advertises is built in the NFS adapter
// (auth.AdvertisedAuthFlavors), which cannot be called from here — it takes a
// runtime share, so importing it would close a cycle. This is therefore a
// second statement of the same rule, and the two are pinned to each other by a
// test in that package rather than kept in step by hand.
//
// The rule: AUTH_UNIX is offered only to a share that allows AUTH_SYS and does
// not mandate Kerberos, and Kerberos pseudoflavors are offered only when the
// server has Kerberos at all. A server that has it always offers at least krb5p,
// which carries the highest protection service and so satisfies any floor a
// share can set — which is why the Kerberos case needs no floor comparison here.
func ExportAcceptsNoAuthFlavor(requireKerberos, allowAuthSys, kerberosEnabled bool) bool {
	if kerberosEnabled {
		return false
	}
	return requireKerberos || !allowAuthSys
}

// ExportNoAuthFlavorCause names the settings that leave an export accepting no
// auth flavor, and the command that repairs them. It is only meaningful when
// ExportAcceptsNoAuthFlavor reports true.
//
// Both flags can be at fault at once, and clearing only one leaves the export
// just as unreachable: re-allowing AUTH_SYS while require_kerberos still stands
// on a server with no Kerberos refuses AUTH_SYS again on the next boot. The
// two-flag case therefore gets a command that sets both, so following the
// remedy actually produces a share that serves.
func ExportNoAuthFlavorCause(shareName string, requireKerberos, allowAuthSys bool) (cause, remedy string) {
	set := "`dfsctl share nfs-config set " + shellQuote(shareName) + " "
	switch {
	case requireKerberos && !allowAuthSys:
		return "require_kerberos is set and allow_auth_sys is off",
			set + "--require-kerberos false --allow-auth-sys true`"
	case requireKerberos:
		return "require_kerberos is set", set + "--require-kerberos false`"
	default:
		return "allow_auth_sys is off", set + "--allow-auth-sys true`"
	}
}

// shellQuote wraps a share name so the remedy above survives being copied into
// a shell. A share name is validated for the handle format, not for the shell:
// it may hold spaces, which split one argument into several, and it may hold
// command substitutions, which a shell would run. Single quotes suspend all of
// that, and the one character they cannot carry is closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
