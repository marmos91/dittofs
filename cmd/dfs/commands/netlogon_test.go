package commands

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/marmos91/dittofs/pkg/config"
)

func TestBuildNetlogonAuthenticator_Disabled(t *testing.T) {
	k := config.KerberosConfig{
		MachineAccount: config.MachineAccountConfig{Enabled: false},
	}
	got, _ := buildNetlogonAuthenticator(k, nil)
	if got != nil {
		t.Fatalf("expected nil for disabled machine account, got %v", got)
	}
}

func TestBuildNetlogonAuthenticator_Enabled(t *testing.T) {
	k := config.KerberosConfig{
		Realm:         "EXAMPLE.COM",
		NetBIOSDomain: "EXAMPLE",
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			Secret:      "s3cr3t",
			DCAddresses: []string{"192.168.1.1"},
		},
	}
	got, _ := buildNetlogonAuthenticator(k, nil)
	if got == nil {
		t.Fatal("expected non-nil authenticator for enabled machine account")
	}
}

func TestBuildNetlogonAuthenticator_EnabledMissingSecret(t *testing.T) {
	k := config.KerberosConfig{
		Realm:         "EXAMPLE.COM",
		NetBIOSDomain: "EXAMPLE",
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			// Secret intentionally empty
			DCAddresses: []string{"192.168.1.1"},
		},
	}
	got, _ := buildNetlogonAuthenticator(k, nil)
	if got != nil {
		t.Fatal("expected nil when Secret is missing")
	}
}

func TestBuildNetlogonAuthenticator_EnabledKeytabOnly(t *testing.T) {
	k := config.KerberosConfig{
		Realm:         "EXAMPLE.COM",
		NetBIOSDomain: "EXAMPLE",
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			// Secret empty, KeytabPath set — not yet supported
			KeytabPath:  "/etc/machine.keytab",
			DCAddresses: []string{"192.168.1.1"},
		},
	}
	got, _ := buildNetlogonAuthenticator(k, nil)
	if got != nil {
		t.Fatal("expected nil when only KeytabPath is set (not yet supported)")
	}
}

func TestBuildNetlogonAuthenticator_EnabledMissingDomain(t *testing.T) {
	k := config.KerberosConfig{
		Realm: "EXAMPLE.COM",
		// NetBIOSDomain intentionally empty
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			Secret:      "s3cr3t",
			DCAddresses: []string{"192.168.1.1"},
		},
	}
	got, _ := buildNetlogonAuthenticator(k, nil)
	if got != nil {
		t.Fatal("expected nil when NetBIOSDomain is missing")
	}
}

func TestBuildNetlogonAuthenticator_EnabledMissingDCAddressesUsesDiscovery(t *testing.T) {
	// No dc_address is valid: the realm drives DNS SRV discovery of the DC at
	// connect time, so passthrough stays enabled (#1324).
	k := config.KerberosConfig{
		Realm:         "EXAMPLE.COM",
		NetBIOSDomain: "EXAMPLE",
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			Secret:      "s3cr3t",
			// DCAddresses intentionally empty — located from the realm.
		},
	}
	got, _ := buildNetlogonAuthenticator(k, nil)
	if got == nil {
		t.Fatal("expected non-nil authenticator when realm is set (DC located via DNS SRV)")
	}
}

func TestBuildNetlogonAuthenticator_EnabledMissingRealm(t *testing.T) {
	// Realm is mandatory: the Kerberos SMB session to the DC and DNS SRV
	// discovery both require it. Without it, passthrough must be disabled (#1324).
	k := config.KerberosConfig{
		// Realm intentionally empty
		NetBIOSDomain: "EXAMPLE",
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			Secret:      "s3cr3t",
			DCAddresses: []string{"192.168.1.1"},
		},
	}
	got, _ := buildNetlogonAuthenticator(k, nil)
	if got != nil {
		t.Fatal("expected nil when realm is missing")
	}
}

// TestBuildNetlogonAuthenticator_OnlineJoinNoSecretNeeded verifies the
// online-join path builds an authenticator + rotation manager without a static
// Secret (the provider owns the password). The provider is lazy, so no DC I/O
// happens at construction.
func TestBuildNetlogonAuthenticator_OnlineJoinNoSecretNeeded(t *testing.T) {
	k := config.KerberosConfig{
		Realm:         "EXAMPLE.COM",
		NetBIOSDomain: "EXAMPLE",
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			// Secret intentionally empty — online join generates it.
			OnlineJoin: config.OnlineJoinConfig{
				Enabled:          true,
				LDAPURL:          "ldaps://dc.example.com",
				BindDN:           "CN=Administrator,CN=Users,DC=example,DC=com",
				BindPassword:     "joinpass",
				BaseDN:           "DC=example,DC=com",
				RotationInterval: 0, // disabled → nil rotation manager
			},
		},
	}
	got, rot := buildNetlogonAuthenticator(k, &fakeSecretStore{})
	if got == nil {
		t.Fatal("expected non-nil authenticator for online-join (no static secret required)")
	}
	if rot != nil {
		t.Fatal("expected nil rotation manager when rotation_interval is 0")
	}
}

// fakeSecretStore is an in-memory netlogon.SecretStore for wiring tests.
type fakeSecretStore struct{ v string }

func (f *fakeSecretStore) GetMachineSecret(context.Context) (string, error) { return f.v, nil }
func (f *fakeSecretStore) SetMachineSecret(_ context.Context, s string) error {
	f.v = s
	return nil
}

var _ netlogon.SecretStore = (*fakeSecretStore)(nil)

func TestKerberosRoundTrip_MachineAccount(t *testing.T) {
	orig := config.KerberosConfig{
		Enabled:       true,
		Realm:         "TEST.LOCAL",
		NetBIOSDomain: "TEST",
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			Secret:      "p@ssw0rd",
			KeytabPath:  "/etc/machine.keytab",
			DCAddresses: []string{"10.0.0.1", "10.0.0.2"},
		},
	}

	dto := kerberosConfigToDTO(orig)
	result := kerberosDTOToConfig(dto)

	ma := result.MachineAccount
	if !ma.Enabled {
		t.Error("MachineAccount.Enabled not preserved")
	}
	if ma.AccountName != orig.MachineAccount.AccountName {
		t.Errorf("AccountName: got %q, want %q", ma.AccountName, orig.MachineAccount.AccountName)
	}
	if ma.Secret != orig.MachineAccount.Secret {
		t.Errorf("Secret: got %q, want %q", ma.Secret, orig.MachineAccount.Secret)
	}
	if ma.KeytabPath != orig.MachineAccount.KeytabPath {
		t.Errorf("KeytabPath: got %q, want %q", ma.KeytabPath, orig.MachineAccount.KeytabPath)
	}
	if len(ma.DCAddresses) != len(orig.MachineAccount.DCAddresses) {
		t.Errorf("DCAddresses: got %v, want %v", ma.DCAddresses, orig.MachineAccount.DCAddresses)
	}
}

// TestMergeMachineAccountFromFile_OverlaysOfflineWhenDBLacksIt covers #1392: a
// DB-sourced Kerberos config with no machine account must inherit the file/env
// offline credential the operator injected, rather than silently dropping it.
func TestMergeMachineAccountFromFile_OverlaysOfflineWhenDBLacksIt(t *testing.T) {
	db := config.KerberosConfig{ // DB row seeded before passthrough was configured
		Enabled:       true,
		NetBIOSDomain: "EXAMPLE",
		Realm:         "EXAMPLE.COM",
	}
	file := config.KerberosConfig{
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DITTOFS$",
			Secret:      "s3cr3t",
			DCAddresses: []string{"dc.example.com"},
		},
	}
	got := mergeMachineAccountFromFile(db, file)
	if !got.MachineAccount.Enabled || got.MachineAccount.AccountName != "DITTOFS$" || got.MachineAccount.Secret != "s3cr3t" {
		t.Fatalf("offline machine account not overlaid from file: %+v", got.MachineAccount)
	}
	// The DB-sourced top-level fields must be preserved.
	if got.NetBIOSDomain != "EXAMPLE" || got.Realm != "EXAMPLE.COM" {
		t.Fatalf("DB-sourced fields clobbered: %+v", got)
	}
}

// TestMergeMachineAccountFromFile_DBMachineAccountWins verifies an API-configured
// machine account (present and enabled in the DB row) is NOT overwritten by the
// file/env credential.
func TestMergeMachineAccountFromFile_DBMachineAccountWins(t *testing.T) {
	db := config.KerberosConfig{
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DBACCOUNT$",
			Secret:      "db-secret",
		},
	}
	file := config.KerberosConfig{
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "FILEACCOUNT$",
			Secret:      "file-secret",
		},
	}
	got := mergeMachineAccountFromFile(db, file)
	if got.MachineAccount.AccountName != "DBACCOUNT$" || got.MachineAccount.Secret != "db-secret" {
		t.Fatalf("DB machine account should win, got: %+v", got.MachineAccount)
	}
}

// TestMergeMachineAccountFromFile_OnlineJoinAlwaysFromFile verifies online-join
// (privileged, never persisted in the DB DTO) is always sourced from file/env,
// even when the DB row carries its own offline machine account.
func TestMergeMachineAccountFromFile_OnlineJoinAlwaysFromFile(t *testing.T) {
	db := config.KerberosConfig{
		MachineAccount: config.MachineAccountConfig{
			Enabled:     true,
			AccountName: "DBACCOUNT$",
			Secret:      "db-secret",
		},
	}
	file := config.KerberosConfig{
		MachineAccount: config.MachineAccountConfig{
			OnlineJoin: config.OnlineJoinConfig{Enabled: true, LDAPURL: "ldaps://dc.example.com"},
		},
	}
	got := mergeMachineAccountFromFile(db, file)
	if !got.MachineAccount.OnlineJoin.Enabled || got.MachineAccount.OnlineJoin.LDAPURL != "ldaps://dc.example.com" {
		t.Fatalf("online-join not overlaid from file: %+v", got.MachineAccount.OnlineJoin)
	}
	// DB offline account still wins for the non-online-join fields.
	if got.MachineAccount.AccountName != "DBACCOUNT$" {
		t.Fatalf("DB offline account clobbered: %+v", got.MachineAccount)
	}
}

// TestOverlayDeploymentPaths_FileWinsWhenSet verifies a file/env keytab,
// krb5.conf, or machine-account keytab path overrides the stale value carried
// in a DB-sourced Kerberos row. This is the deployment-relocation case: an
// operator moves the mount and the DB row must not pin the old absolute path.
func TestOverlayDeploymentPaths_FileWinsWhenSet(t *testing.T) {
	db := config.KerberosConfig{
		KeytabPath: "/etc/dittofs/krb5.keytab",
		Krb5Conf:   "/etc/krb5.conf",
		MachineAccount: config.MachineAccountConfig{
			KeytabPath: "/etc/dittofs/machine.keytab",
		},
	}
	file := config.KerberosConfig{
		KeytabPath: "/kerberos/dittofs.keytab",
		Krb5Conf:   "/kerberos-krb5/krb5.conf",
		MachineAccount: config.MachineAccountConfig{
			KeytabPath: "/kerberos/machine.keytab",
		},
	}
	got := overlayDeploymentPaths(db, file)
	if got.KeytabPath != "/kerberos/dittofs.keytab" {
		t.Fatalf("KeytabPath not overlaid: %q", got.KeytabPath)
	}
	if got.Krb5Conf != "/kerberos-krb5/krb5.conf" {
		t.Fatalf("Krb5Conf not overlaid: %q", got.Krb5Conf)
	}
	if got.MachineAccount.KeytabPath != "/kerberos/machine.keytab" {
		t.Fatalf("MachineAccount.KeytabPath not overlaid: %q", got.MachineAccount.KeytabPath)
	}
}

// TestOverlayDeploymentPaths_EmptyFileKeepsDB verifies a pure-API deployment
// (no path in file/env) is unaffected: the DB row's paths are preserved.
func TestOverlayDeploymentPaths_EmptyFileKeepsDB(t *testing.T) {
	db := config.KerberosConfig{
		KeytabPath: "/etc/dittofs/krb5.keytab",
		Krb5Conf:   "/etc/krb5.conf",
		MachineAccount: config.MachineAccountConfig{
			KeytabPath: "/etc/dittofs/machine.keytab",
		},
	}
	got := overlayDeploymentPaths(db, config.KerberosConfig{})
	if got.KeytabPath != "/etc/dittofs/krb5.keytab" {
		t.Fatalf("KeytabPath clobbered by empty file: %q", got.KeytabPath)
	}
	if got.Krb5Conf != "/etc/krb5.conf" {
		t.Fatalf("Krb5Conf clobbered by empty file: %q", got.Krb5Conf)
	}
	if got.MachineAccount.KeytabPath != "/etc/dittofs/machine.keytab" {
		t.Fatalf("MachineAccount.KeytabPath clobbered by empty file: %q", got.MachineAccount.KeytabPath)
	}
}
