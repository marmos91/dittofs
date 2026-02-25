package identity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ============================================================================
// Mock MappingStore
// ============================================================================

type mockMappingStore struct {
	mappings map[string]*IdentityMapping
	getErr   error
}

func newMockMappingStore() *mockMappingStore {
	return &mockMappingStore{
		mappings: make(map[string]*IdentityMapping),
	}
}

func (s *mockMappingStore) GetMapping(_ context.Context, principal string) (*IdentityMapping, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	m, ok := s.mappings[principal]
	if !ok {
		return nil, nil
	}
	return m, nil
}

func (s *mockMappingStore) ListMappings(_ context.Context) ([]*IdentityMapping, error) {
	var result []*IdentityMapping
	for _, m := range s.mappings {
		result = append(result, m)
	}
	return result, nil
}

func (s *mockMappingStore) CreateMapping(_ context.Context, mapping *IdentityMapping) error {
	s.mappings[mapping.Principal] = mapping
	return nil
}

func (s *mockMappingStore) DeleteMapping(_ context.Context, principal string) error {
	delete(s.mappings, principal)
	return nil
}

// ============================================================================
// TableMapper tests
// ============================================================================

func TestTableMapper_FoundMapping(t *testing.T) {
	store := newMockMappingStore()
	store.mappings["alice@EXAMPLE.COM"] = &IdentityMapping{
		Principal: "alice@EXAMPLE.COM",
		Username:  "alice",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	lookup := func(_ context.Context, username string) (*ResolvedIdentity, error) {
		if username == "alice" {
			return &ResolvedIdentity{
				Username: "alice",
				UID:      1000,
				GID:      1000,
				GIDs:     []uint32{1000},
				Found:    true,
			}, nil
		}
		return &ResolvedIdentity{Found: false}, nil
	}

	m := NewTableMapper(store, lookup)
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
	if result.Domain != "EXAMPLE.COM" {
		t.Fatalf("expected Domain=EXAMPLE.COM, got %s", result.Domain)
	}
}

func TestTableMapper_MissingMapping(t *testing.T) {
	store := newMockMappingStore()

	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		t.Fatal("lookup should not be called when mapping not found")
		return nil, nil
	}

	m := NewTableMapper(store, lookup)
	result, err := m.Resolve(context.Background(), "unknown@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatal("expected Found=false for missing mapping")
	}
}

func TestTableMapper_StoreError(t *testing.T) {
	store := newMockMappingStore()
	storeErr := errors.New("database connection lost")
	store.getErr = storeErr

	m := NewTableMapper(store, nil)
	_, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err == nil {
		t.Fatal("expected error from store failure")
	}
	if !errors.Is(err, storeErr) {
		t.Fatalf("expected storeErr, got %v", err)
	}
}

func TestTableMapper_UserLookupFailure(t *testing.T) {
	store := newMockMappingStore()
	store.mappings["alice@EXAMPLE.COM"] = &IdentityMapping{
		Principal: "alice@EXAMPLE.COM",
		Username:  "alice",
	}

	lookupErr := errors.New("user service unavailable")
	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		return nil, lookupErr
	}

	m := NewTableMapper(store, lookup)
	_, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err == nil {
		t.Fatal("expected error from user lookup failure")
	}
	if !errors.Is(err, lookupErr) {
		t.Fatalf("expected lookupErr, got %v", err)
	}
}

func TestTableMapper_MappedUserNotInControlPlane(t *testing.T) {
	store := newMockMappingStore()
	store.mappings["alice@EXAMPLE.COM"] = &IdentityMapping{
		Principal: "alice@EXAMPLE.COM",
		Username:  "alice",
	}

	lookup := func(_ context.Context, _ string) (*ResolvedIdentity, error) {
		return &ResolvedIdentity{Found: false}, nil
	}

	m := NewTableMapper(store, lookup)
	result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatal("expected Found=false when mapped user not in control plane")
	}
}
