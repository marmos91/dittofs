package identity

import (
	"context"
	"testing"
)

func TestStaticMapper_KnownPrincipal(t *testing.T) {
	cfg := &StaticMapperConfig{
		StaticMap: map[string]StaticIdentity{
			"alice@EXAMPLE.COM": {UID: 1000, GID: 1000, GIDs: []uint32{1000, 1001}},
		},
		DefaultUID: 65534,
		DefaultGID: 65534,
	}

	m := NewStaticMapper(cfg)
	result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Found {
		t.Fatal("expected Found=true")
	}
	if result.UID != 1000 {
		t.Fatalf("expected UID=1000, got %d", result.UID)
	}
	if result.GID != 1000 {
		t.Fatalf("expected GID=1000, got %d", result.GID)
	}
	if len(result.GIDs) != 2 {
		t.Fatalf("expected 2 GIDs, got %d", len(result.GIDs))
	}
	if result.GIDs[0] != 1000 || result.GIDs[1] != 1001 {
		t.Fatalf("unexpected GIDs: %v", result.GIDs)
	}
	if result.Username != "alice" {
		t.Fatalf("expected Username=alice, got %s", result.Username)
	}
	if result.Domain != "EXAMPLE.COM" {
		t.Fatalf("expected Domain=EXAMPLE.COM, got %s", result.Domain)
	}
}

func TestStaticMapper_UnknownPrincipalGetsDefaults(t *testing.T) {
	cfg := &StaticMapperConfig{
		StaticMap:  map[string]StaticIdentity{},
		DefaultUID: 65534,
		DefaultGID: 65534,
	}

	m := NewStaticMapper(cfg)
	result, err := m.Resolve(context.Background(), "unknown@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Found {
		t.Fatal("expected Found=true (static mapper always finds)")
	}
	if result.UID != 65534 {
		t.Fatalf("expected UID=65534, got %d", result.UID)
	}
	if result.GID != 65534 {
		t.Fatalf("expected GID=65534, got %d", result.GID)
	}
	if result.Username != "unknown" {
		t.Fatalf("expected Username=unknown, got %s", result.Username)
	}
}

func TestStaticMapper_NilStaticMap(t *testing.T) {
	cfg := &StaticMapperConfig{
		StaticMap:  nil,
		DefaultUID: 65534,
		DefaultGID: 65534,
	}

	m := NewStaticMapper(cfg)
	result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Found {
		t.Fatal("expected Found=true")
	}
	if result.UID != 65534 {
		t.Fatalf("expected UID=65534, got %d", result.UID)
	}
}

func TestStaticMapper_GIDsAreCopied(t *testing.T) {
	cfg := &StaticMapperConfig{
		StaticMap: map[string]StaticIdentity{
			"alice@EXAMPLE.COM": {UID: 1000, GID: 1000, GIDs: []uint32{100, 200}},
		},
		DefaultUID: 65534,
		DefaultGID: 65534,
	}

	m := NewStaticMapper(cfg)
	result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Modify the returned GIDs to verify they're a copy
	result.GIDs[0] = 999

	// Resolve again and check original is unchanged
	result2, _ := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if result2.GIDs[0] != 100 {
		t.Fatal("GIDs were not deep copied - modifying result affected source")
	}
}
