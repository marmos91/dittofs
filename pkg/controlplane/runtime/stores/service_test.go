package stores_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/stores"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newMemStore builds an in-memory MetadataStore usable as a test fixture for
// the registry-level methods under test. We intentionally use the
// shipping memory engine rather than a one-off stub so the type-assertion
// branches in SwapMetadataStore / ListPostgresRestoreOrphans exercise
// against a real engine type.
func newMemStore() *memory.MemoryMetadataStore {
	return memory.NewMemoryMetadataStoreWithDefaults()
}

func TestSwapMetadataStore_UnregisteredName(t *testing.T) {
	svc := stores.New()
	_, err := svc.SwapMetadataStore("ghost", newMemStore())
	if err == nil {
		t.Fatalf("expected error for unregistered name, got nil")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected 'not registered' in error, got %v", err)
	}
}

func TestSwapMetadataStore_NilNewStore(t *testing.T) {
	svc := stores.New()
	if err := svc.RegisterMetadataStore("A", newMemStore()); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := svc.SwapMetadataStore("A", nil)
	if err == nil {
		t.Fatalf("expected error for nil newStore, got nil")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Fatalf("expected 'nil' in error, got %v", err)
	}
}

func TestSwapMetadataStore_EmptyName(t *testing.T) {
	svc := stores.New()
	_, err := svc.SwapMetadataStore("", newMemStore())
	if err == nil {
		t.Fatalf("expected error for empty name, got nil")
	}
	if !strings.Contains(err.Error(), "empty name") {
		t.Fatalf("expected 'empty name' in error, got %v", err)
	}
}

func TestSwapMetadataStore_HappyPath(t *testing.T) {
	svc := stores.New()
	a := newMemStore()
	b := newMemStore()
	if err := svc.RegisterMetadataStore("X", a); err != nil {
		t.Fatalf("register: %v", err)
	}
	old, err := svc.SwapMetadataStore("X", b)
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	if old != a {
		t.Fatalf("expected displaced store to be the original A instance")
	}
	got, err := svc.GetMetadataStore("X")
	if err != nil {
		t.Fatalf("get after swap: %v", err)
	}
	if got != b {
		t.Fatalf("expected registered store to be the new B instance")
	}
}

func TestSwapMetadataStore_DoesNotCloseOldStore(t *testing.T) {
	// The swap contract delegates Close() to the caller — we do NOT call
	// Close() on the displaced store inside SwapMetadataStore. The caller
	// (Phase 5 CommitSwap) owns close + backing-path cleanup.
	svc := stores.New()
	a := newMemStore()
	b := newMemStore()
	if err := svc.RegisterMetadataStore("X", a); err != nil {
		t.Fatalf("register: %v", err)
	}
	old, err := svc.SwapMetadataStore("X", b)
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	// The displaced store must still be usable — a proxy for "not closed".
	if id := old.(*memory.MemoryMetadataStore).GetStoreID(); id == "" {
		t.Fatalf("displaced store unexpectedly unusable after swap")
	}
}

func TestOpenMetadataStoreAtPath_Memory(t *testing.T) {
	svc := stores.New()
	cfg := &models.MetadataStoreConfig{Type: "memory", Name: "mem-test"}
	got, err := svc.OpenMetadataStoreAtPath(context.Background(), cfg, "/ignored")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got == nil {
		t.Fatalf("returned nil store")
	}
	// Must NOT be registered as a side effect.
	if _, err := svc.GetMetadataStore("mem-test"); err == nil {
		t.Fatalf("OpenMetadataStoreAtPath must NOT register the store")
	}
}

func TestOpenMetadataStoreAtPath_NilConfig(t *testing.T) {
	svc := stores.New()
	_, err := svc.OpenMetadataStoreAtPath(context.Background(), nil, "/tmp")
	if err == nil {
		t.Fatalf("expected error for nil cfg, got nil")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Fatalf("expected 'nil' in error, got %v", err)
	}
}

func TestOpenMetadataStoreAtPath_UnknownType(t *testing.T) {
	svc := stores.New()
	cfg := &models.MetadataStoreConfig{Type: "scylla", Name: "novel"}
	_, err := svc.OpenMetadataStoreAtPath(context.Background(), cfg, "")
	if err == nil {
		t.Fatalf("expected error for unknown type, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected 'unsupported' in error, got %v", err)
	}
}

func TestOpenMetadataStoreAtPath_BadgerRequiresPath(t *testing.T) {
	svc := stores.New()
	cfg := &models.MetadataStoreConfig{Type: "badger", Name: "bad-test"}
	_, err := svc.OpenMetadataStoreAtPath(context.Background(), cfg, "")
	if err == nil {
		t.Fatalf("expected error for empty pathOverride on badger, got nil")
	}
	if !strings.Contains(err.Error(), "pathOverride") {
		t.Fatalf("expected 'pathOverride' in error, got %v", err)
	}
}

func TestOpenMetadataStoreAtPath_PostgresRequiresPath(t *testing.T) {
	svc := stores.New()
	cfg := &models.MetadataStoreConfig{Type: "postgres", Name: "pg-test"}
	_, err := svc.OpenMetadataStoreAtPath(context.Background(), cfg, "")
	if err == nil {
		t.Fatalf("expected error for empty pathOverride on postgres, got nil")
	}
	if !strings.Contains(err.Error(), "pathOverride") {
		t.Fatalf("expected 'pathOverride' in error, got %v", err)
	}
}

func TestListPostgresRestoreOrphans_StoreNotFound(t *testing.T) {
	svc := stores.New()
	_, err := svc.ListPostgresRestoreOrphans(context.Background(), "missing", "public_restore_")
	if err == nil {
		t.Fatalf("expected error for missing store, got nil")
	}
}

func TestListPostgresRestoreOrphans_NonPostgresStore(t *testing.T) {
	svc := stores.New()
	if err := svc.RegisterMetadataStore("mem-pg-mock", newMemStore()); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := svc.ListPostgresRestoreOrphans(context.Background(), "mem-pg-mock", "public_restore_")
	if err == nil {
		t.Fatalf("expected error for non-Postgres store, got nil")
	}
	if !strings.Contains(err.Error(), "schema enumeration") {
		t.Fatalf("expected 'schema enumeration' in error, got %v", err)
	}
}

func TestDropPostgresSchema_NonPostgresStore(t *testing.T) {
	svc := stores.New()
	if err := svc.RegisterMetadataStore("mem-pg-mock", newMemStore()); err != nil {
		t.Fatalf("register: %v", err)
	}
	err := svc.DropPostgresSchema(context.Background(), "mem-pg-mock", "some_schema")
	if err == nil {
		t.Fatalf("expected error for non-Postgres store, got nil")
	}
	if !strings.Contains(err.Error(), "schema drop") {
		t.Fatalf("expected 'schema drop' in error, got %v", err)
	}
}

func TestDropPostgresSchema_StoreNotFound(t *testing.T) {
	svc := stores.New()
	err := svc.DropPostgresSchema(context.Background(), "missing", "some_schema")
	if err == nil {
		t.Fatalf("expected error for missing store, got nil")
	}
}

// TestSwapMetadataStore_ConcurrentDifferentNames sanity-checks that two
// concurrent swaps on distinct names complete without deadlock. Swap holds
// the registry write lock, so there is no inter-swap parallelism — what we
// verify is that the serialized swaps both return success.
func TestSwapMetadataStore_ConcurrentDifferentNames(t *testing.T) {
	svc := stores.New()
	a1 := newMemStore()
	a2 := newMemStore()
	b1 := newMemStore()
	b2 := newMemStore()
	if err := svc.RegisterMetadataStore("A", a1); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if err := svc.RegisterMetadataStore("B", b1); err != nil {
		t.Fatalf("register B: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := svc.SwapMetadataStore("A", a2)
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := svc.SwapMetadataStore("B", b2)
		errCh <- err
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent swap: %v", err)
		}
	}
	gotA, _ := svc.GetMetadataStore("A")
	gotB, _ := svc.GetMetadataStore("B")
	if gotA != a2 {
		t.Fatalf("A not swapped to new instance")
	}
	if gotB != b2 {
		t.Fatalf("B not swapped to new instance")
	}
}
