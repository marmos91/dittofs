// Package kerberos provides the Kerberos AuthProvider implementation for DittoFS.
//
// The Provider type implements the auth.AuthProvider interface and manages:
//   - Keytab and krb5.conf loading with environment variable overrides
//   - Hot-reload capability for keytab rotation
//   - SPNEGO/Kerberos token detection for the auth provider chain
//
// This package does NOT contain RPCSEC_GSS wire protocol logic (see
// internal/adapter/nfs/rpc/gss/) or the GSS context state machine.
// Protocol-specific code in the NFS and SMB adapters uses the Provider
// to access keytab state, while handling wire-level authentication directly.
//
// Configuration is defined in pkg/config.KerberosConfig to avoid circular imports.
// This package accepts *config.KerberosConfig as constructor parameter.
//
// References:
//   - RFC 2203: RPCSEC_GSS Protocol Specification
//   - RFC 4121: The Kerberos Version 5 GSS-API Mechanism
package kerberos
