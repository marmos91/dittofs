package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestReset_EmptyStore verifies that Reset on a freshly-constructed
// store is a no-op that returns nil and leaves the store usable.
func TestReset_EmptyStore(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()

	r, ok := any(store).(metadata.Resetable)
	if !ok {
		t.Fatal("*MemoryMetadataStore must implement metadata.Resetable")
	}

	if err := r.Reset(t.Context()); err != nil {
		t.Fatalf("Reset on empty store: %v", err)
	}

	shares, err := store.ListShares(t.Context())
	if err != nil {
		t.Fatalf("ListShares after Reset: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("ListShares after Reset = %v, want empty", shares)
	}
}

// TestReset_PopulatedStore verifies that Reset on a populated store
// clears all share/file state.
func TestReset_PopulatedStore(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := t.Context()

	// Populate: one share, two files.
	const shareName = "/reset-populated"
	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	rootAttr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	for _, name := range []string{"a.bin", "b.bin"} {
		handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
		if err != nil {
			t.Fatalf("GenerateHandle %s: %v", name, err)
		}
		_, id, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			t.Fatalf("DecodeFileHandle %s: %v", name, err)
		}
		f := &metadata.File{
			ID:        id,
			ShareName: shareName,
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Mode: 0o644,
			},
		}
		if err := store.PutFile(ctx, f); err != nil {
			t.Fatalf("PutFile %s: %v", name, err)
		}
		if err := store.SetParent(ctx, handle, rootHandle); err != nil {
			t.Fatalf("SetParent %s: %v", name, err)
		}
		if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
			t.Fatalf("SetChild %s: %v", name, err)
		}
	}

	r := any(store).(metadata.Resetable)
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares after Reset: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("ListShares after Reset = %v, want empty", shares)
	}
}

// TestReset_PreservesConfig verifies that storeID is NOT cleared by Reset
// (DATA vs CONFIG split — storeID is the engine-persistent identifier).
func TestReset_PreservesConfig(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := t.Context()

	wantStoreID := store.GetStoreID()
	if wantStoreID == "" {
		t.Fatal("pre-Reset store ID is empty")
	}

	r := any(store).(metadata.Resetable)
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	gotStoreID := store.GetStoreID()
	if gotStoreID != wantStoreID {
		t.Fatalf("storeID changed across Reset: got %q, want %q", gotStoreID, wantStoreID)
	}
}

// TestReset_CtxCancellation verifies that a pre-cancelled context causes
// Reset to return a wrapped ctx.Err() without mutating store state.
func TestReset_CtxCancellation(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := any(store).(metadata.Resetable)
	err := r.Reset(ctx)
	if err == nil {
		t.Fatal("Reset with cancelled ctx: err = nil, want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reset with cancelled ctx: err = %v, want errors.Is(context.Canceled)", err)
	}
}

// TestReset_ReusableAfterReset verifies that Reset leaves the store in a
// usable state: a new share can be created and queried without nil-map
// panics or stale state from prior population.
func TestReset_ReusableAfterReset(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := t.Context()

	// Populate, Reset, then re-populate using the SAME store instance.
	const firstShare = "/before-reset"
	if err := store.CreateShare(ctx, &metadata.Share{Name: firstShare}); err != nil {
		t.Fatalf("CreateShare first: %v", err)
	}

	r := any(store).(metadata.Resetable)
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	const secondShare = "/after-reset"
	if err := store.CreateShare(ctx, &metadata.Share{Name: secondShare}); err != nil {
		t.Fatalf("CreateShare after Reset: %v", err)
	}
	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(shares) != 1 || shares[0] != secondShare {
		t.Fatalf("post-reset ListShares = %v, want [%q]", shares, secondShare)
	}
}
