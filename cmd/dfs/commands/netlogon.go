package commands

import (
	"context"
	"log/slog"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// machineSecretKey is the control-plane settings key under which the online-join
// provider persists the (rotated) machine-account password.
const machineSecretKey = "netlogon.machine_account.secret"

// machineSecretStore adapts the control-plane SettingsStore to the
// netlogon.SecretStore interface, scoping all reads/writes to machineSecretKey.
// This keeps the netlogon package free of any control-plane dependency.
type machineSecretStore struct {
	settings store.SettingsStore
}

// newMachineSecretStore wraps the control-plane store for machine-secret
// persistence. Returns nil when s is nil so the offline path passes nil through.
func newMachineSecretStore(s store.SettingsStore) netlogon.SecretStore {
	if s == nil {
		return nil
	}
	return &machineSecretStore{settings: s}
}

func (m *machineSecretStore) GetMachineSecret(ctx context.Context) (string, error) {
	return m.settings.GetSetting(ctx, machineSecretKey)
}

func (m *machineSecretStore) SetMachineSecret(ctx context.Context, secret string) error {
	return m.settings.SetSetting(ctx, machineSecretKey, secret)
}

// buildNetlogonAuthenticator creates a NetlogonAuthenticator from the Kerberos
// machine-account configuration. Returns (nil, nil) when MachineAccount.Enabled
// is false (or the config is incomplete), so the SMB handler falls back to local
// NTLM authentication without a domain controller.
//
// It also returns a *netlogon.RotationManager when the online-join provider is
// active (nil otherwise); the caller starts it and stops it on shutdown.
//
// The offline path is backed by a netlogon.MutableProvider so the machine
// credential / DC binding can be hot-reloaded over the API without a restart
// (#1325). The online-join path (#1323) owns its own credential lifecycle via
// the RotationManager.
//
// secret is the persistence backend for the online-join machine password; it
// may be nil for the offline path (which never persists anything new).
//
// The concrete *netlogon.Authenticator is returned (not the narrow
// NetlogonAuthenticator interface) because the SMB adapter needs its
// ReloadCredential/Close methods for the #1325 hot-reload and shutdown. The
// disabled path returns a nil *Authenticator, so callers can test `auth == nil`
// directly.
func buildNetlogonAuthenticator(k config.KerberosConfig, secret netlogon.SecretStore) (*netlogon.Authenticator, *netlogon.RotationManager) {
	if !k.MachineAccount.Enabled {
		return nil, nil
	}
	ma := k.MachineAccount

	// Online-join (opt-in): the provider creates the computer object and owns the
	// machine-password lifecycle, so it does not require a pre-supplied secret and
	// is handled before the offline secret validation.
	if ma.OnlineJoin.Enabled {
		if ma.AccountName == "" {
			slog.Warn("NETLOGON machine account is enabled but AccountName is not set; NTLM passthrough disabled")
			return nil, nil
		}
		if k.NetBIOSDomain == "" {
			slog.Warn("NETLOGON machine account is enabled but kerberos.netbios_domain (DomainName) is not set; NTLM passthrough disabled")
			return nil, nil
		}
		if k.Realm == "" {
			slog.Warn("NETLOGON machine account is enabled but kerberos.realm is not set (required for the Kerberos SMB session to the DC and for DNS SRV discovery); NTLM passthrough disabled")
			return nil, nil
		}

		// Derive the NetBIOS workstation name. MS-NRPC §3.1.4.1 requires the short
		// host name without the trailing '$' machine-account marker.
		workstation := netbiosWorkstation(k)

		oj := ma.OnlineJoin
		cfg := netlogon.OnlineConfig{
			AccountName:      ma.AccountName,
			Workstation:      workstation,
			DomainName:       k.NetBIOSDomain,
			Realm:            k.Realm,
			DCAddresses:      ma.DCAddresses,
			RotationInterval: oj.RotationInterval,
			Join: netlogon.JoinConfig{
				LDAPURL:      oj.LDAPURL,
				StartTLS:     oj.StartTLS,
				BindDN:       oj.BindDN,
				BindPassword: oj.BindPassword,
				BaseDN:       oj.BaseDN,
				OU:           oj.OU,
				MachineName:  workstation,
				DNSHostName:  oj.DNSHostName,
				SPNs:         oj.SPNs,
				TLS: netlogon.JoinTLSConfig{
					CACertFile:         oj.CACertFile,
					InsecureSkipVerify: oj.InsecureSkipVerify,
				},
			},
		}
		provider := netlogon.NewOnlineProvider(cfg, secret)
		auth := netlogon.NewAuthenticator(provider)
		rot := netlogon.NewRotationManager(provider, auth, oj.RotationInterval)
		slog.Info("NETLOGON machine account: online-join provider active",
			"account", ma.AccountName, "ldap_url", oj.LDAPURL, "rotation_interval", oj.RotationInterval)
		return auth, rot
	}

	// Offline path: a static admin-supplied secret, wrapped in a MutableProvider so
	// the credential can be hot-reloaded over the API without a restart (#1325).
	cred, ok := netlogonCredentialFromConfig(k)
	if !ok {
		return nil, nil
	}
	slog.Info("NETLOGON machine account: offline provider active",
		"account", ma.AccountName, "dc_addresses", ma.DCAddresses)
	return netlogon.NewAuthenticator(netlogon.NewMutableProvider(cred)), nil
}

// netlogonCredentialFromConfig validates the machine-account sub-block and, when
// it is enabled and complete, returns the derived MachineCredential. The bool is
// false (and a warning is logged) when passthrough must stay disabled. Shared by
// the startup build (offline path) and the seed of the runtime's hot-reloadable
// credential.
func netlogonCredentialFromConfig(k config.KerberosConfig) (netlogon.MachineCredential, bool) {
	if !k.MachineAccount.Enabled {
		return netlogon.MachineCredential{}, false
	}

	ma := k.MachineAccount

	// Online-join is wired separately in buildNetlogonAuthenticator, where its own
	// provider generates, persists, and rotates the machine password. This function
	// derives only the OFFLINE static credential, so for online-join it returns
	// (zero, false) meaning "no offline credential to seed" — NOT "pass-through
	// disabled". The caller (start.go) consequently seeds a nil runtime hot-reload
	// credential, which is correct: that credential drives the MutableProvider
	// hot-reload path, and online-join does not use it (its provider is not a
	// MutableProvider — a ReloadCredential just drops the cached channel and the
	// online provider re-supplies the password). Returning quietly here also avoids
	// the misleading "Secret is not set" warnings below.
	if ma.OnlineJoin.Enabled {
		return netlogon.MachineCredential{}, false
	}

	// Validate required fields before constructing a doomed credential.
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
