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
