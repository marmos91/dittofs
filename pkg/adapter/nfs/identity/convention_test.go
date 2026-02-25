package identity

import (
	"context"
	"errors"
	"testing"
)

// ============================================================================
// ConventionMapper tests
// ============================================================================

func TestConventionMapper_MatchingRealm(t *testing.T) {
	lookup := func(_ context.Context, username string) (*ResolvedIdentity, error) {
		if username == "alice" {
			return &ResolvedIdentity{
				Username: "alice",
				UID:      1000,
				GID:      1000,
				GIDs:     []uint32{1000, 1001},
				Found:    true,
			}, nil
		}
		return &ResolvedIdentity{Found: false}, nil
	}

	m := NewConventionMapper("EXAMPLE.COM", lookup)
	result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Found {
		t.Fatal("expected Found=true")
	}
	if result.Username != "alice" {
		t.Fatalf("expected Username=alice, got %s", result.Username)
	}
	if result.UID != 1000 {
		t.Fatalf("expected UID=1000, got %d", result.UID)
	}
	if result.GID != 1000 {
		t.Fatalf("expected GID=1000, got %d", result.GID)
	}
	if result.Domain != "EXAMPLE.COM" {
		t.Fatalf("expected Domain=EXAMPLE.COM, got %s", result.Domain)
	}
	if len(result.GIDs) != 2 {
		t.Fatalf("expected 2 GIDs, got %d", len(result.GIDs))
	}
}

func TestConventionMapper_NonMatchingRealm(t *testing.T) {
	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		t.Fatal("lookup should not be called for non-matching realm")
		return nil, nil
	}

	m := NewConventionMapper("EXAMPLE.COM", lookup)
	result, err := m.Resolve(context.Background(), "alice@OTHER.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatal("expected Found=false for non-matching realm")
	}
}

func TestConventionMapper_CaseInsensitiveDomain(t *testing.T) {
	lookup := func(_ context.Context, username string) (*ResolvedIdentity, error) {
		return &ResolvedIdentity{
			Username: username,
			UID:      1000,
			GID:      1000,
			Found:    true,
		}, nil
	}

	m := NewConventionMapper("EXAMPLE.COM", lookup)

	// Lower case should match upper case realm
	result, err := m.Resolve(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Found {
		t.Fatal("expected Found=true for case-insensitive domain match")
	}
	if result.Domain != "example.com" {
		t.Fatalf("expected Domain=example.com, got %s", result.Domain)
	}
}

func TestConventionMapper_NumericUID(t *testing.T) {
	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		t.Fatal("lookup should not be called for numeric UID")
		return nil, nil
	}

	m := NewConventionMapper("EXAMPLE.COM", lookup)
	result, err := m.Resolve(context.Background(), "1000@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Found {
		t.Fatal("expected Found=true for numeric UID")
	}
	if result.UID != 1000 {
		t.Fatalf("expected UID=1000, got %d", result.UID)
	}
	if result.GID != 1000 {
		t.Fatalf("expected GID=1000, got %d", result.GID)
	}
	if result.Username != "1000" {
		t.Fatalf("expected Username=1000, got %s", result.Username)
	}
	if result.Domain != "EXAMPLE.COM" {
		t.Fatalf("expected Domain=EXAMPLE.COM, got %s", result.Domain)
	}
}

func TestConventionMapper_EmptyDomain(t *testing.T) {
	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		t.Fatal("lookup should not be called for empty domain")
		return nil, nil
	}

	m := NewConventionMapper("EXAMPLE.COM", lookup)
	result, err := m.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatal("expected Found=false for empty domain")
	}
}

func TestConventionMapper_UserLookupFailure(t *testing.T) {
	lookupErr := errors.New("database unavailable")
	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		return nil, lookupErr
	}

	m := NewConventionMapper("EXAMPLE.COM", lookup)
	_, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err == nil {
		t.Fatal("expected error from user lookup failure")
	}
	if !errors.Is(err, lookupErr) {
		t.Fatalf("expected lookupErr, got %v", err)
	}
}

func TestConventionMapper_UserNotFound(t *testing.T) {
	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		return &ResolvedIdentity{Found: false}, nil
	}

	m := NewConventionMapper("EXAMPLE.COM", lookup)
	result, err := m.Resolve(context.Background(), "unknown@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatal("expected Found=false for unknown user")
	}
}

func TestConventionMapper_UserLookupReturnsNil(t *testing.T) {
	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		return nil, nil
	}

	m := NewConventionMapper("EXAMPLE.COM", lookup)
	result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatal("expected Found=false when lookup returns nil")
	}
}
