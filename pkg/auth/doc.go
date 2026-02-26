// Package auth provides centralized authentication abstractions for DittoFS.
//
// This package defines the core types and interfaces for authentication:
//
//   - AuthProvider: Pluggable authentication mechanism (Kerberos, NTLM, etc.)
//   - Authenticator: Chains AuthProviders, tries each in order
//   - AuthResult: Authentication outcome with Identity
//   - Identity: Protocol-neutral authenticated identity
//   - IdentityMapper: Converts AuthResult to protocol-specific identity
//
// Sub-packages:
//   - kerberos/: Kerberos AuthProvider with keytab management and hot-reload
//
// Protocol adapters (NFS, SMB) implement IdentityMapper to convert auth results
// into their protocol-specific identity contexts. The Authenticator chains
// providers so that multiple mechanisms can be tried in order.
package auth
