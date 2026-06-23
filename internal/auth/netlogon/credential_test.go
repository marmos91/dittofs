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
