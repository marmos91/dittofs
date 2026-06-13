package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestReconcileINV02_DuplicateHashRowsVisible pins finding I-1: reconcileINV02
// must sum FileBlock.RefCount per FileBlock-ID, not per distinct content hash.
//
// It seeds one file referencing two distinct FileBlock rows that carry the
// SAME content hash but DISTINCT IDs (the legal post-dedup-copy shape), each
// with RefCount=3. The file's FileAttr.Blocks lists both as independent refs.
//
// Before the I-1 fix, reconcileINV02 walked distinct hashes via
// EnumerateFileBlocks + GetByHash, which returns ANY one row for the shared
// hash — so totalRefCount counted a single row (3) instead of both (6),
// silently hiding an inflated/leaked RefCount on the sibling row.
//
// After the fix, the per-ID walk counts both rows: totalRefCount == 6.
func TestReconcileINV02_DuplicateHashRowsVisible(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	const shareName = "inv02-dup-hash"
	rootHandle := createTestShare(t, store, shareName)

	// Two FileBlock rows sharing one hash but with distinct IDs under a single
	// payloadID prefix, so ListFileBlocks(payloadID) recovers BOTH.
	const payloadID = "dup-block"
	sharedHash := hashOfSeed("inv02-dup-shared-hash")
	now := time.Now()

	refs := make([]block.BlockRef, 0, 2)
	for i := 0; i < 2; i++ {
		blockID := payloadID + "/" + string(rune('0'+i))
		fb := &block.FileBlock{
			ID:            blockID,
			Hash:          sharedHash,
			State:         block.BlockStateRemote,
			LocalPath:     "/cache/" + blockID,
			BlockStoreKey: "cas/shared/" + sharedHash.String(),
			DataSize:      4096,
			RefCount:      3,
			LastAccess:    now,
			CreatedAt:     now,
		}
		if err := store.Put(ctx, fb); err != nil {
			t.Fatalf("Put(%s): %v", blockID, err)
		}
		refs = append(refs, block.BlockRef{
			Hash:   sharedHash,
			Offset: uint64(i) * 4096,
			Size:   4096,
		})
	}

	name := "dup.bin"
	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	file := &metadata.File{
		ID:        fileID,
		ShareName: shareName,
		Path:      "/" + name,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			UID:       1000,
			GID:       1000,
			Mtime:     now,
			Ctime:     now,
			Atime:     now,
			Blocks:    refs,
			PayloadID: metadata.PayloadID(payloadID),
		},
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
		t.Fatalf("SetChild: %v", err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount: %v", err)
	}

	totalRefs, totalRefCount, err := reconcileINV02(ctx, store, shareName)
	if err != nil {
		t.Fatalf("reconcileINV02: %v", err)
	}

	// Two BlockRef entries -> totalRefs == 2.
	if totalRefs != 2 {
		t.Errorf("totalRefs = %d, want 2 (two BlockRef entries)", totalRefs)
	}
	// Both duplicate-hash rows must contribute their RefCount independently:
	// 3 + 3 == 6. A per-distinct-hash sum would report only 3.
	if totalRefCount != 6 {
		t.Fatalf("totalRefCount = %d, want 6 (3+3 across two duplicate-hash rows); "+
			"per-distinct-hash dedup would hide the sibling row's RefCount", totalRefCount)
	}
}

// nonProviderStore wraps a real metadata.Store but is a distinct named type
// that deliberately does NOT expose DurableHandleStore(), so a type assertion
// to DurableHandleStoreProvider fails. Embedding the interface promotes the
// full metadata.Store surface so the factory still produces a usable store.
type nonProviderStore struct {
	metadata.Store
}

// TestDurableHandleStore_NonImplementingStoreSkips pins finding I-3:
// getDurableStore must t.Skip (not panic) when the store does not implement
// DurableHandleStoreProvider.
//
// Before the fix, getDurableStore used a bare one-value type assertion that
// panicked on a non-implementing store, aborting the whole test binary. Every
// sub-test helper (testDurablePutAndGet, etc.) calls getDurableStore directly
// and bypassed the two-value guard in RunDurableHandleStoreTests, so the panic
// was reachable in practice. After the fix, getDurableStore itself skips.
//
// The sub-test invokes getDurableStore directly — the exact call site the fix
// hardens — so the bare-assertion regression panics here and the two-value
// form skips cleanly.
func TestDurableHandleStore_NonImplementingStoreSkips(t *testing.T) {
	factory := func(t *testing.T) metadata.Store {
		return &nonProviderStore{Store: memory.NewMemoryMetadataStoreWithDefaults()}
	}

	// Public entry: must not panic; it skips at the top-level guard.
	RunDurableHandleStoreTests(t, factory)
}

// TestGetDurableStore_NonImplementingStoreSkips drives getDurableStore directly
// — the per-sub-test accessor that bypassed the top-level guard in
// RunDurableHandleStoreTests. This is the exact call site finding I-3 hardens:
// a bare type assertion panics here (aborting the binary), while the fixed
// two-value form calls t.Skip (runtime.Goexit), so the t.Fatalf below is never
// reached.
func TestGetDurableStore_NonImplementingStoreSkips(t *testing.T) {
	factory := func(t *testing.T) metadata.Store {
		return &nonProviderStore{Store: memory.NewMemoryMetadataStoreWithDefaults()}
	}

	store := getDurableStore(t, factory)
	t.Fatalf("getDurableStore returned %v; expected a skip on a non-implementing store", store)
}
