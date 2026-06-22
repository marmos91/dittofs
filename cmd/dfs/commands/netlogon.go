package commands

import (
	"log/slog"
	"os"
	"strings"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/marmos91/dittofs/pkg/config"
)

// buildNetlogonAuthenticator creates a NetlogonAuthenticator from the Kerberos
// machine-account configuration. Returns nil (typed-nil-safe: an untyped nil
// interface value) when MachineAccount.Enabled is false, so the SMB handler
// falls back to local NTLM authentication without a domain controller.
//
// Typed-nil-interface safety: the disabled path returns the bare nil literal so
// callers can safely test `auth == nil` without hitting the typed-nil-interface
// trap (a (*netlogon.Authenticator)(nil) wrapped in the interface would not
// equal nil).
func buildNetlogonAuthenticator(k config.KerberosConfig) netlogon.NetlogonAuthenticator {
	if !k.MachineAccount.Enabled {
		return nil
	}

	// Derive the NetBIOS workstation name.  MS-NRPC §3.1.4.1 requires the
	// short host name without the trailing '$' machine-account marker.
	workstation := netbiosWorkstation(k)

	cred := netlogon.MachineCredential{
		AccountName: k.MachineAccount.AccountName,
		Password:    k.MachineAccount.Secret,
		Workstation: workstation,
		DomainName:  k.NetBIOSDomain,
		Realm:       k.Realm,
		DCAddresses: k.MachineAccount.DCAddresses,
	}
	return netlogon.NewAuthenticator(netlogon.NewOfflineProvider(cred))
}

// netbiosWorkstation derives the short workstation name used in NETLOGON RPC
// calls.  Priority:
//  1. AccountName with a trailing '$' stripped (the AD machine-account
//     convention, e.g. "DITTOFS$" → "DITTOFS").
//  2. Short hostname from os.Hostname() (everything before the first '.').
func netbiosWorkstation(k config.KerberosConfig) string {
	if n := k.MachineAccount.AccountName; n != "" {
		return strings.TrimSuffix(n, "$")
	}
	if h, err := os.Hostname(); err == nil {
		if idx := strings.IndexByte(h, '.'); idx > 0 {
			return h[:idx]
		}
		return h
	}
	slog.Warn("NETLOGON workstation name could not be derived; DC may reject the logon")
	return ""
}
