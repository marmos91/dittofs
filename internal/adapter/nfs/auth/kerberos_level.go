package auth

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// requiredGSSServiceForLevel maps a share's min_kerberos_level to the minimum
// RPCSEC_GSS service level (RFC 2203 §5.2.3) it requires. hasFloor is false for
// an empty or unrecognised level, in which case no floor is enforced.
func requiredGSSServiceForLevel(minLevel string) (svc uint32, hasFloor bool) {
	switch minLevel {
	case models.KerberosLevelKrb5:
		return gss.RPCGSSSvcNone, true
	case models.KerberosLevelKrb5i:
		return gss.RPCGSSSvcIntegrity, true
	case models.KerberosLevelKrb5p:
		return gss.RPCGSSSvcPrivacy, true
	default:
		return 0, false
	}
}

// MeetsMinKerberosLevel reports whether an RPCSEC_GSS request that negotiated
// the `negotiated` service level (one of gss.RPCGSSSvc*) satisfies the share's
// configured min_kerberos_level floor.
//
// The RPCSEC_GSS service levels are ordered none(1) < integrity(2) <
// privacy(3), so a numeric >= comparison enforces the floor. An empty or
// unrecognised min_kerberos_level means no floor is configured and the request
// is allowed.
//
// This is the wire-protection counterpart to RequireKerberos: RequireKerberos
// rejects non-GSS auth flavors, MeetsMinKerberosLevel rejects GSS sessions
// negotiated below the requested protection level (e.g. a plain `krb5`
// authentication-only session on a share that demands `krb5p` privacy). It is
// only meaningful for RPCSEC_GSS requests; non-GSS flavors are governed by
// AllowAuthSys / RequireKerberos.
func MeetsMinKerberosLevel(minLevel string, negotiated uint32) bool {
	required, hasFloor := requiredGSSServiceForLevel(minLevel)
	if !hasFloor {
		return true
	}
	return negotiated >= required
}
