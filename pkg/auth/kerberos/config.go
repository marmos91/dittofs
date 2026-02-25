// Package kerberos provides the shared Kerberos authentication layer for DittoFS.
//
// This package wraps the gokrb5 library to provide:
//   - Keytab and krb5.conf loading with environment variable overrides
//   - Hot-reload capability for keytab rotation
//   - Identity mapping from Kerberos principals to Unix UID/GID
//
// The package does NOT contain RPCSEC_GSS wire protocol logic (see
// internal/adapter/nfs/rpc/gss/) or the GSS context state machine
// (implemented in subsequent plans).
//
// Configuration is defined in pkg/config.KerberosConfig to avoid circular imports.
// This package accepts *config.KerberosConfig as constructor parameter.
//
// References:
//   - RFC 2203: RPCSEC_GSS Protocol Specification
//   - RFC 4121: The Kerberos Version 5 GSS-API Mechanism
package kerberos
