package auth

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// protectionForGSSService maps a negotiated RPCSEC_GSS service level (RFC 2203
// §5.2.3, one of gss.RPCGSSSvc*) to the protocol-neutral Kerberos protection
// the share policy compares against. The service levels are ordered
// none(1) < integrity(2) < privacy(3), and anything at or above a level is
// treated as that level. A value below none is no service at all, so it meets
// no floor.
func protectionForGSSService(svc uint32) runtime.KerberosProtection {
	switch {
	case svc < gss.RPCGSSSvcNone:
		return runtime.NoKerberos
	case svc >= gss.RPCGSSSvcPrivacy:
		return runtime.KerberosPrivacy
	case svc >= gss.RPCGSSSvcIntegrity:
		return runtime.KerberosIntegrity
	default:
		return runtime.KerberosAuthOnly
	}
}

// MeetsMinKerberosLevel reports whether an RPCSEC_GSS request that negotiated
// the `negotiated` service level (one of gss.RPCGSSSvc*) satisfies the share's
// configured min_kerberos_level floor. An empty or unrecognised
// min_kerberos_level means no floor is configured and the request is allowed.
//
// This is the wire-protection counterpart to RequireKerberos: RequireKerberos
// rejects non-GSS auth flavors, MeetsMinKerberosLevel rejects GSS sessions
// negotiated below the requested protection level (e.g. a plain `krb5`
// authentication-only session on a share that demands `krb5p` privacy). It is
// only meaningful for RPCSEC_GSS requests; non-GSS flavors are governed by
// AllowAuthSys / RequireKerberos.
func MeetsMinKerberosLevel(minLevel string, negotiated uint32) bool {
	return protectionForGSSService(negotiated) >= runtime.RequiredKerberosProtection(minLevel)
}
