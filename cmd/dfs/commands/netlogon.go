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

	// Validate required fields before constructing a doomed authenticator.
	ma := k.MachineAccount
	if ma.AccountName == "" {
		slog.Warn("NETLOGON machine account is enabled but AccountName is not set; NTLM passthrough disabled")
		return nil
	}
	if ma.Secret == "" {
		if ma.KeytabPath != "" {
			slog.Warn("NETLOGON machine account: keytab-based credentials are not yet supported (tracked separately); NTLM passthrough disabled",
				"keytab_path", ma.KeytabPath)
		} else {
			slog.Warn("NETLOGON machine account is enabled but Secret is not set; NTLM passthrough disabled")
		}
		return nil
	}
	if k.NetBIOSDomain == "" {
		slog.Warn("NETLOGON machine account is enabled but kerberos.netbios_domain (DomainName) is not set; NTLM passthrough disabled")
		return nil
	}
	if len(ma.DCAddresses) == 0 {
		slog.Warn("NETLOGON machine account is enabled but no DCAddresses are configured; NTLM passthrough disabled")
		return nil
	}

	// Derive the NetBIOS workstation name.  MS-NRPC §3.1.4.1 requires the
	// short host name without the trailing '$' machine-account marker.
	workstation := netbiosWorkstation(k)

	cred := netlogon.MachineCredential{
		AccountName: ma.AccountName,
		Password:    ma.Secret,
		Workstation: workstation,
		DomainName:  k.NetBIOSDomain,
		Realm:       k.Realm,
		DCAddresses: ma.DCAddresses,
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
