package commands

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/config"
)

func TestBuildNetlogonAuthenticator_Disabled(t *testing.T) {
	k := config.KerberosConfig{
		MachineAccount: config.MachineAccountConfig{Enabled: false},
	}
	got := buildNetlogonAuthenticator(k)
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
	got := buildNetlogonAuthenticator(k)
	if got == nil {
		t.Fatal("expected non-nil authenticator for enabled machine account")
	}
}

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
