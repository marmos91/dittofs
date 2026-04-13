package kerberos

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/identity"
)

func aliceLookup(_ context.Context, username string) (*identity.ResolvedIdentity, error) {
	users := map[string]*identity.ResolvedIdentity{
		"alice": {Username: "alice", UID: 1000, GID: 1000, Found: true},
		"bob":   {Username: "bob", UID: 2000, GID: 2000, Found: true},
	}
	if r, ok := users[username]; ok {
		return r, nil
	}
	return &identity.ResolvedIdentity{Found: false}, nil
}

type memLinkStore struct {
	links map[string]string // "provider|externalID" -> username
}

func (m *memLinkStore) GetLink(_ context.Context, provider, externalID string) (string, bool, error) {
	key := provider + "|" + externalID
	if u, ok := m.links[key]; ok {
		return u, true, nil
	}
	return "", false, nil
}

func (m *memLinkStore) ListLinks(context.Context, string) ([]identity.IdentityLink, error) {
	return nil, nil
}
func (m *memLinkStore) CreateLink(context.Context, identity.IdentityLink) error { return nil }
func (m *memLinkStore) DeleteLink(context.Context, string, string) error        { return nil }
func (m *memLinkStore) ListLinksForUser(context.Context, string) ([]identity.IdentityLink, error) {
	return nil, nil
}

func TestProvider_ExplicitMapping(t *testing.T) {
	store := &memLinkStore{links: map[string]string{
		"kerberos|admin@CORP.COM": "alice",
	}}
	p := New("EXAMPLE.COM", store, aliceLookup)

	result, err := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "admin@CORP.COM",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Username != "alice" || result.UID != 1000 {
		t.Fatalf("expected alice/1000, got %+v", result)
	}
}

func TestProvider_ConventionRealmMatch(t *testing.T) {
	p := New("EXAMPLE.COM", nil, aliceLookup)

	result, err := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "alice@EXAMPLE.COM",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Username != "alice" || result.UID != 1000 {
		t.Fatalf("expected alice/1000, got %+v", result)
	}
	if result.Domain != "EXAMPLE.COM" {
		t.Fatalf("expected domain EXAMPLE.COM, got %s", result.Domain)
	}
}

func TestProvider_ConventionCaseInsensitive(t *testing.T) {
	p := New("EXAMPLE.COM", nil, aliceLookup)

	result, _ := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "alice@example.com",
	})
	if !result.Found {
		t.Fatal("expected case-insensitive realm match")
	}
}

func TestProvider_RealmMismatch(t *testing.T) {
	p := New("EXAMPLE.COM", nil, aliceLookup)

	result, _ := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "alice@OTHER.COM",
	})
	if result.Found {
		t.Fatal("expected Found=false for realm mismatch")
	}
}

func TestProvider_NoDomain(t *testing.T) {
	p := New("EXAMPLE.COM", nil, aliceLookup)

	result, _ := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "alice",
	})
	if result.Found {
		t.Fatal("expected Found=false for bare username")
	}
}

func TestProvider_UnknownUser(t *testing.T) {
	p := New("EXAMPLE.COM", nil, aliceLookup)

	result, _ := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "unknown@EXAMPLE.COM",
	})
	if result.Found {
		t.Fatal("expected Found=false for unknown user")
	}
}

func TestProvider_NumericUID(t *testing.T) {
	p := New("EXAMPLE.COM", nil, aliceLookup)

	result, _ := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "1000@EXAMPLE.COM",
	})
	if !result.Found || result.UID != 1000 {
		t.Fatalf("expected numeric UID 1000, got %+v", result)
	}
}

func TestProvider_CanResolve(t *testing.T) {
	p := New("EXAMPLE.COM", nil, aliceLookup)

	tests := []struct {
		cred *identity.Credential
		want bool
	}{
		{&identity.Credential{Provider: "kerberos", ExternalID: "alice@EXAMPLE.COM"}, true},
		{&identity.Credential{Provider: "oidc", ExternalID: "alice@EXAMPLE.COM"}, false},
		{&identity.Credential{ExternalID: "alice@EXAMPLE.COM"}, true},
		{&identity.Credential{ExternalID: "alice"}, false},
	}

	for _, tt := range tests {
		if got := p.CanResolve(tt.cred); got != tt.want {
			t.Errorf("CanResolve(%v) = %v, want %v", tt.cred, got, tt.want)
		}
	}
}

func TestProvider_ExplicitMappingTakesPrecedence(t *testing.T) {
	store := &memLinkStore{links: map[string]string{
		"kerberos|alice@EXAMPLE.COM": "bob",
	}}
	p := New("EXAMPLE.COM", store, aliceLookup)

	result, _ := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "alice@EXAMPLE.COM",
	})
	if !result.Found || result.Username != "bob" {
		t.Fatalf("expected explicit mapping to bob, got %+v", result)
	}
}

func TestProvider_StoreError(t *testing.T) {
	dbErr := errors.New("db down")
	store := &memLinkStore{}
	store.links = nil // will cause no match
	p := New("EXAMPLE.COM", store, func(_ context.Context, _ string) (*identity.ResolvedIdentity, error) {
		return nil, dbErr
	})

	// Convention path hits userLookup which fails
	_, err := p.Resolve(context.Background(), &identity.Credential{
		Provider:   "kerberos",
		ExternalID: "alice@EXAMPLE.COM",
	})
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected db error, got %v", err)
	}
}

func TestProvider_SpecialPrincipals(t *testing.T) {
	p := New("EXAMPLE.COM", nil, aliceLookup)

	for _, principal := range []string{"OWNER@", "GROUP@", "EVERYONE@"} {
		result, _ := p.Resolve(context.Background(), &identity.Credential{
			Provider:   "kerberos",
			ExternalID: principal,
		})
		if result.Found {
			t.Errorf("expected Found=false for special principal %s", principal)
		}
	}
}
