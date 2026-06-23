package commands

import (
	"log/slog"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/marmos91/dittofs/pkg/config"
)

// buildNetlogonAuthenticator creates a *netlogon.Authenticator from the Kerberos
// machine-account configuration. Returns nil when MachineAccount.Enabled is
// false (or the config is incomplete), so the SMB handler falls back to local
// NTLM authentication without a domain controller.
//
// The authenticator is backed by a netlogon.MutableProvider so the machine
// credential / DC binding can be hot-reloaded over the API without a restart
// (#1325): an identity-provider config change updates the runtime credential and
// the SMB adapter calls Authenticator.ReloadCredential.
func buildNetlogonAuthenticator(k config.KerberosConfig) *netlogon.Authenticator {
	cred, ok := netlogonCredentialFromConfig(k)
	if !ok {
		return nil
	}
	return netlogon.NewAuthenticator(netlogon.NewMutableProvider(cred))
}

// netlogonCredentialFromConfig validates the machine-account sub-block and, when
// it is enabled and complete, returns the derived MachineCredential. The bool is
// false (and a warning is logged) when passthrough must stay disabled. Shared by
// the startup build and the seed of the runtime's hot-reloadable credential.
func netlogonCredentialFromConfig(k config.KerberosConfig) (netlogon.MachineCredential, bool) {
	if !k.MachineAccount.Enabled {
		return netlogon.MachineCredential{}, false
	}

	// Validate required fields before constructing a doomed credential.
	ma := k.MachineAccount
	if ma.AccountName == "" {
		slog.Warn("NETLOGON machine account is enabled but AccountName is not set; NTLM passthrough disabled")
		return netlogon.MachineCredential{}, false
	}
	if ma.Secret == "" {
		if ma.KeytabPath != "" {
			slog.Warn("NETLOGON machine account: keytab-based credentials are not yet supported (tracked separately); NTLM passthrough disabled",
				"keytab_path", ma.KeytabPath)
		} else {
			slog.Warn("NETLOGON machine account is enabled but Secret is not set; NTLM passthrough disabled")
		}
		return netlogon.MachineCredential{}, false
	}
	if k.NetBIOSDomain == "" {
		slog.Warn("NETLOGON machine account is enabled but kerberos.netbios_domain (DomainName) is not set; NTLM passthrough disabled")
		return netlogon.MachineCredential{}, false
	}
	// The realm is mandatory: the secure channel authenticates to the DC over a
	// Kerberos SMB session, and when no dc_address is set the realm also drives
	// DNS SRV discovery. A dc_address is optional — absent one, the DC is located
	// from the realm via _ldap._tcp.dc._msdcs.<realm> (#1324).
	if k.Realm == "" {
		slog.Warn("NETLOGON machine account is enabled but kerberos.realm is not set (required for the Kerberos SMB session to the DC and for DNS SRV discovery); NTLM passthrough disabled")
		return netlogon.MachineCredential{}, false
	}

	return netlogon.BuildMachineCredential(ma.AccountName, ma.Secret, k.NetBIOSDomain, k.Realm, ma.DCAddresses), true
}

// netbiosWorkstation derives the short workstation name used in NETLOGON RPC
// calls from the machine-account name (falling back to the OS hostname).
func netbiosWorkstation(k config.KerberosConfig) string {
	return netlogon.DeriveWorkstation(k.MachineAccount.AccountName)
}
