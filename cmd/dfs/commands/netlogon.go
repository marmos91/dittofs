package commands

import (
	"context"
	"log/slog"
	"os"
	"strings"

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
// machine-account configuration. Returns nil (typed-nil-safe: an untyped nil
// interface value) when MachineAccount.Enabled is false, so the SMB handler
// falls back to local NTLM authentication without a domain controller.
//
// It also returns a *netlogon.RotationManager when the online-join provider is
// active and rotation is configured (nil otherwise); the caller starts it and
// stops it on shutdown. Returning (nil, nil) is the disabled case.
//
// secret is the persistence backend for the online-join machine password; it
// may be nil for the offline path (which never persists anything new).
//
// Typed-nil-interface safety: the disabled path returns the bare nil literal so
// callers can safely test `auth == nil` without hitting the typed-nil-interface
// trap (a (*netlogon.Authenticator)(nil) wrapped in the interface would not
// equal nil).
func buildNetlogonAuthenticator(k config.KerberosConfig, secret netlogon.SecretStore) (netlogon.NetlogonAuthenticator, *netlogon.RotationManager) {
	if !k.MachineAccount.Enabled {
		return nil, nil
	}

	ma := k.MachineAccount
	if ma.AccountName == "" {
		slog.Warn("NETLOGON machine account is enabled but AccountName is not set; NTLM passthrough disabled")
		return nil, nil
	}
	if k.NetBIOSDomain == "" {
		slog.Warn("NETLOGON machine account is enabled but kerberos.netbios_domain (DomainName) is not set; NTLM passthrough disabled")
		return nil, nil
	}
	// The realm is mandatory: the secure channel authenticates to the DC over a
	// Kerberos SMB session, and when no dc_address is set the realm also drives
	// DNS SRV discovery. A dc_address is optional — absent one, the DC is located
	// from the realm via _ldap._tcp.dc._msdcs.<realm> (#1324).
	if k.Realm == "" {
		slog.Warn("NETLOGON machine account is enabled but kerberos.realm is not set (required for the Kerberos SMB session to the DC and for DNS SRV discovery); NTLM passthrough disabled")
		return nil, nil
	}

	// Derive the NetBIOS workstation name.  MS-NRPC §3.1.4.1 requires the
	// short host name without the trailing '$' machine-account marker.
	workstation := netbiosWorkstation(k)

	// Online-join (opt-in): the provider creates the computer object and owns the
	// machine-password lifecycle. Otherwise the offline provider uses the
	// admin-supplied static secret.
	if ma.OnlineJoin.Enabled {
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

	// Offline path: a static admin-supplied secret is required.
	if ma.Secret == "" {
		if ma.KeytabPath != "" {
			slog.Warn("NETLOGON machine account: keytab-based credentials are not yet supported (tracked separately); NTLM passthrough disabled",
				"keytab_path", ma.KeytabPath)
		} else {
			slog.Warn("NETLOGON machine account is enabled but Secret is not set (and online_join is off); NTLM passthrough disabled")
		}
		return nil, nil
	}

	cred := netlogon.MachineCredential{
		AccountName: ma.AccountName,
		Password:    ma.Secret,
		Workstation: workstation,
		DomainName:  k.NetBIOSDomain,
		Realm:       k.Realm,
		DCAddresses: ma.DCAddresses,
	}
	return netlogon.NewAuthenticator(netlogon.NewOfflineProvider(cred)), nil
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
