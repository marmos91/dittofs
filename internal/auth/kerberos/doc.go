// Package kerberos provides the shared Kerberos service layer used by both
// NFS RPCSEC_GSS and SMB SESSION_SETUP for AP-REQ verification, session key
// extraction, AP-REP mutual authentication, and authenticator replay detection.
//
// The KerberosService wraps the pkg/auth/kerberos Provider and adds:
//   - AP-REQ verification via gokrb5 service.VerifyAPREQ
//   - Authenticator subkey preference (per MS-SMB2 3.3.5.5.3 and RFC 4120)
//   - AP-REP construction for mutual authentication (raw, not GSS-wrapped)
//   - Cross-protocol replay detection via ReplayCache
//
// Protocol-specific framing (GSS-API wrapping for NFS, SPNEGO for SMB) is
// handled by the respective protocol adapters, not by this package.
package kerberos
