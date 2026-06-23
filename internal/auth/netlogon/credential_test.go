package netlogon

import (
	"context"
	"testing"
)

func TestOfflineProviderReturnsCredential(t *testing.T) {
	p := NewOfflineProvider(MachineCredential{
		AccountName: "DITTOFS$", Password: "secret",
		Workstation: "DITTOFS", DomainName: "DITTOFS", Realm: "DITTOFS.AD",
		DCAddresses: []string{"dc1.dittofs.ad"},
	})
	got, err := p.Credential(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AccountName != "DITTOFS$" || len(got.DCAddresses) != 1 {
		t.Fatalf("unexpected credential: %+v", got)
	}
}

func TestOfflineProviderValidates(t *testing.T) {
	p := NewOfflineProvider(MachineCredential{AccountName: "DITTOFS$"})
	if _, err := p.Credential(context.Background()); err == nil {
		t.Fatal("expected validation error for incomplete credential")
	}
}

// TestOfflineProviderRealmDiscoveryFallback verifies a credential with no DC
// address is accepted when a realm is present, since the secure channel can
// then locate a DC via DNS SRV discovery (#1324).
func TestOfflineProviderRealmDiscoveryFallback(t *testing.T) {
	p := NewOfflineProvider(MachineCredential{
		AccountName: "DITTOFS$", Password: "secret",
		Workstation: "DITTOFS", DomainName: "DITTOFS", Realm: "DITTOFS.AD",
		// DCAddresses intentionally empty: discovery resolves it from the realm.
	})
	got, err := p.Credential(context.Background())
	if err != nil {
		t.Fatalf("expected realm-only credential to validate, got error: %v", err)
	}
	if len(got.DCAddresses) != 0 || got.Realm != "DITTOFS.AD" {
		t.Fatalf("unexpected credential: %+v", got)
	}
}

// TestOfflineProviderRejectsNoDCNoRealm verifies a credential with neither a DC
// address nor a realm is rejected: nothing can locate a DC (#1324).
func TestOfflineProviderRejectsNoDCNoRealm(t *testing.T) {
	p := NewOfflineProvider(MachineCredential{
		AccountName: "DITTOFS$", Password: "secret",
		Workstation: "DITTOFS", DomainName: "DITTOFS",
		// No DCAddresses, no Realm.
	})
	if _, err := p.Credential(context.Background()); err == nil {
		t.Fatal("expected validation error when neither DC address nor realm is set")
	}
}

// TestMutableProviderSetSwapsCredential verifies Set atomically replaces the
// credential returned by Credential (the basis of NETLOGON hot-reload, #1325).
func TestMutableProviderSetSwapsCredential(t *testing.T) {
	p := NewMutableProvider(MachineCredential{
		AccountName: "OLD$", Password: "secret",
		DomainName: "DITTOFS", Realm: "DITTOFS.AD",
	})
	got, err := p.Credential(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AccountName != "OLD$" {
		t.Fatalf("expected OLD$, got %q", got.AccountName)
	}

	p.Set(MachineCredential{
		AccountName: "NEW$", Password: "secret2",
		DomainName: "DITTOFS", Realm: "DITTOFS.AD",
	})
	got, err = p.Credential(context.Background())
	if err != nil {
		t.Fatalf("unexpected error after Set: %v", err)
	}
	if got.AccountName != "NEW$" || got.Password != "secret2" {
		t.Fatalf("Set did not swap credential: %+v", got)
	}
}

// TestMutableProviderValidates verifies Credential rejects an incomplete swapped
// credential, matching the offline provider's contract.
func TestMutableProviderValidates(t *testing.T) {
	p := NewMutableProvider(MachineCredential{AccountName: "DITTOFS$"})
	if _, err := p.Credential(context.Background()); err == nil {
		t.Fatal("expected validation error for incomplete credential")
	}
}

// TestDeriveWorkstation covers the account-name and hostname-fallback paths.
func TestDeriveWorkstation(t *testing.T) {
	if got := DeriveWorkstation("DITTOFS$"); got != "DITTOFS" {
		t.Fatalf("expected DITTOFS, got %q", got)
	}
	if got := DeriveWorkstation("PLAIN"); got != "PLAIN" {
		t.Fatalf("expected PLAIN, got %q", got)
	}
}

// TestBuildMachineCredential verifies the shared assembler derives the
// workstation and copies the fields used by both startup and hot-reload.
func TestBuildMachineCredential(t *testing.T) {
	c := BuildMachineCredential("DITTOFS$", "pw", "DITTOFS", "DITTOFS.AD", []string{"10.0.0.1"})
	if c.AccountName != "DITTOFS$" || c.Password != "pw" || c.DomainName != "DITTOFS" ||
		c.Realm != "DITTOFS.AD" || c.Workstation != "DITTOFS" || len(c.DCAddresses) != 1 {
		t.Fatalf("unexpected credential: %+v", c)
	}
}
