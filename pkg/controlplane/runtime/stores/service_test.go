package stores_test

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime/stores"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestRegistry covers the registry surface end to end: registration rejects
// nil stores, empty names and duplicates; lookup reports a missing name; and
// listing/counting reflect what was registered.
func TestRegistry(t *testing.T) {
	svc := stores.New()
	store := memory.NewMemoryMetadataStoreWithDefaults()

	if err := svc.RegisterMetadataStore("", store); err == nil {
		t.Fatal("empty name must be rejected")
	}
	if err := svc.RegisterMetadataStore("a", nil); err == nil {
		t.Fatal("nil store must be rejected")
	}
	if err := svc.RegisterMetadataStore("a", store); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.RegisterMetadataStore("a", memory.NewMemoryMetadataStoreWithDefaults()); err == nil {
		t.Fatal("duplicate name must be rejected")
	}

	got, err := svc.GetMetadataStore("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != store {
		t.Fatal("get returned a different instance")
	}
	if _, err := svc.GetMetadataStore("ghost"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing name must report not found, got %v", err)
	}

	if names := svc.ListMetadataStores(); len(names) != 1 || names[0] != "a" {
		t.Fatalf("list = %v, want [a]", names)
	}
	if n := svc.CountMetadataStores(); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}

	// Closing is best-effort and must not panic on a store that closes cleanly.
	svc.CloseMetadataStores()
}
