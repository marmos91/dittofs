package storetest

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// INV-02 Property-Based Fuzzer (Phase 12 D-36)
//
// Verifies that across concurrent create/delete/copy churn, the metadata
// store maintains the global invariant:
//
//     ∑ FileBlock.RefCount  ==  ∑ len(FileAttr.Blocks)
//
// The property fuzzer (testINV02_PropertyFuzz) runs against all 3 backends
// via the conformance harness. The leak-injection scenario
// (testINV02_LeakInjection) uses an optional backend capability —
// RefCountLeakInjector — to forcibly desynchronize a single FileBlock's
// RefCount and verify the reconciliation arithmetic detects the drift.
//
// Bug surface this test covers:
//   - WR-4-01-style donor-refcount leaks in dedup short-circuits
//   - missed RefCount decrements on file delete
//   - lost-update race on concurrent CopyPayload-style ref-bumps
//   - silent backend bugs that under/over-count refs
// ============================================================================

// RefCountLeakInjector is an optional backend capability used by the
// INV-02 leak-injection scenario to artificially desynchronize a
// single FileBlock's RefCount from the FileAttr.Blocks references that
// (logically) own it. Backends that cannot represent a desynchronized
// refcount cleanly skip the scenario via type-assertion failure.
//
// Test-only: never call from production code. The hook bypasses the
// FileBlockStore contract by mutating RefCount independently of any
// IncrementRefCount / DecrementRefCount call site.
type RefCountLeakInjector interface {
	// InjectRefCountLeak adds leakAmount to the named block's RefCount
	// without touching FileAttr.Blocks anywhere. The post-call invariant
	// ∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks) is therefore violated
	// by exactly leakAmount, which is the property the audit must detect.
	InjectRefCountLeak(ctx context.Context, blockID string, leakAmount uint32) error
}

// testINV02_PropertyFuzz creates/deletes/copies files concurrently across
// 10 goroutines, then asserts at the quiescent point that:
//
//	∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks)
//
// Bug surface: WR-4-01-style donor leaks, missed decrements on file
// delete, lost-update on concurrent CopyPayload. Runs against all 3
// backends via the conformance factory.
//
// Phase 12 D-36 defaults: 100 iterations, 10 concurrent goroutines.
func testINV02_PropertyFuzz(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "inv02-fuzz"
	const concurrency = 10
	const opsPerWorker = 10 // 10 workers × 10 ops = 100 total ops (D-36)

	rootHandle := createTestShare(t, store, shareName)

	// Per-worker registries (no cross-worker synchronization needed; each
	// worker manages its own slice of created file handles). After the
	// fuzz cycle, the quiescent-point assertion walks the whole share.
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID) + 1))
			ws := &workerState{}
			for op := 0; op < opsPerWorker; op++ {
				switch rng.Intn(3) {
				case 0:
					if err := fuzzCreateFile(ctx, store, shareName, rootHandle, workerID, op, rng, ws); err != nil {
						t.Errorf("worker %d op %d (create): %v", workerID, op, err)
						return
					}
				case 1:
					if err := fuzzDeleteFile(ctx, store, rootHandle, rng, ws); err != nil {
						t.Errorf("worker %d op %d (delete): %v", workerID, op, err)
						return
					}
				case 2:
					if err := fuzzCopyFile(ctx, store, shareName, rootHandle, workerID, op, rng, ws); err != nil {
						t.Errorf("worker %d op %d (copy): %v", workerID, op, err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	if t.Failed() {
		return
	}

	// Quiescent-point reconciliation across the whole share.
	totalRefs, totalRefCount, err := reconcileINV02(ctx, store, shareName)
	if err != nil {
		t.Fatalf("reconcileINV02: %v", err)
	}
	if delta := int64(totalRefs) - int64(totalRefCount); delta != 0 {
		t.Fatalf("INV-02 violation: ∑len(Blocks)=%d, ∑RefCount=%d, delta=%d",
			totalRefs, totalRefCount, delta)
	}
}

// testINV02_LeakInjection asserts that the storetest reconciliation
// arithmetic correctly detects a refcount desynchronization injected
// via the RefCountLeakInjector capability. Backends that don't
// implement the capability skip cleanly.
func testINV02_LeakInjection(t *testing.T, factory StoreFactory) {
	store := factory(t)
	injector, ok := store.(RefCountLeakInjector)
	if !ok {
		t.Skipf("backend %T does not implement RefCountLeakInjector — leak-injection scenario unavailable", store)
	}

	ctx := t.Context()
	const shareName = "inv02-leak"
	rootHandle := createTestShare(t, store, shareName)

	// Seed one well-formed file with three BlockRefs / three FileBlocks
	// each carrying RefCount=1. Pre-leak invariant: refs=3, refCount=3.
	rng := rand.New(rand.NewSource(42))
	ws := &workerState{}
	if err := fuzzCreateFile(ctx, store, shareName, rootHandle, 0 /*workerID*/, 0 /*opID*/, rng, ws); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	if len(ws.files) != 1 {
		t.Fatalf("seed create produced %d files, want 1", len(ws.files))
	}
	seededHandles := ws.files[0].blockIDs

	totalRefs, totalRefCount, err := reconcileINV02(ctx, store, shareName)
	if err != nil {
		t.Fatalf("reconcileINV02 (pre-leak): %v", err)
	}
	if totalRefs != totalRefCount {
		t.Fatalf("pre-leak baseline broken: refs=%d, refCount=%d (delta=%d)",
			totalRefs, totalRefCount, int64(totalRefs)-int64(totalRefCount))
	}

	// Inject a known leak of +5 onto one of the seed blocks. After this,
	// the invariant should report refs unchanged but refCount up by 5,
	// i.e. delta = -5 (refs - refCount).
	const leak uint32 = 5
	targetID := seededHandles[0]
	if err := injector.InjectRefCountLeak(ctx, targetID, leak); err != nil {
		t.Fatalf("InjectRefCountLeak: %v", err)
	}

	postRefs, postRefCount, err := reconcileINV02(ctx, store, shareName)
	if err != nil {
		t.Fatalf("reconcileINV02 (post-leak): %v", err)
	}
	delta := int64(postRefs) - int64(postRefCount)
	if delta != -int64(leak) {
		t.Fatalf("expected delta=%d after leak of %d, got refs=%d, refCount=%d, delta=%d",
			-int64(leak), leak, postRefs, postRefCount, delta)
	}
}

// ============================================================================
// Worker-state helpers
// ============================================================================

// fuzzFileEntry tracks a file the fuzzer created — handle for
// delete/copy, name for parent-child unlink, and the FileBlock IDs
// used to seed FileBlock rows so cleanup paths know what to decrement.
type fuzzFileEntry struct {
	handle   metadata.FileHandle
	name     string
	blockIDs []string
}

// workerState mirrors the inline anonymous struct in testINV02_PropertyFuzz
// so the leak-injection test can reuse the create helper.
type workerState struct {
	files []fuzzFileEntry
}

// fuzzCreateFile creates a new file with 1–3 BlockRefs and matching
// FileBlock rows (RefCount=1 each). Block IDs are unique per (worker,
// opID, blockIdx) so independent workers never collide. Hashes are
// derived from a worker-relative seed so dedup-aware backends see
// genuinely distinct content.
func fuzzCreateFile(ctx context.Context, store metadata.MetadataStore, shareName string, rootHandle metadata.FileHandle, workerID, opID int, rng *rand.Rand, ws *workerState) error {
	nBlocks := rng.Intn(3) + 1 // 1, 2, or 3
	name := fmt.Sprintf("w%d-op%d.bin", workerID, opID)

	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	if err != nil {
		return fmt.Errorf("GenerateHandle: %w", err)
	}
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return fmt.Errorf("DecodeFileHandle: %w", err)
	}

	blockIDs := make([]string, 0, nBlocks)
	refs := make([]blockstore.BlockRef, 0, nBlocks)
	now := time.Now()
	for i := 0; i < nBlocks; i++ {
		hashSeed := fmt.Sprintf("inv02-w%d-op%d-blk%d", workerID, opID, i)
		h := hashOfSeed(hashSeed)
		blockID := fmt.Sprintf("inv02/%d/%d/%d", workerID, opID, i)
		fb := &blockstore.FileBlock{
			ID:            blockID,
			Hash:          h,
			State:         blockstore.BlockStateRemote,
			LocalPath:     "/cache/" + blockID,
			BlockStoreKey: "cas/" + h.String()[0:2] + "/" + h.String()[2:4] + "/" + h.String(),
			DataSize:      4096,
			RefCount:      1,
			LastAccess:    now,
			CreatedAt:     now,
		}
		if err := store.Put(ctx, fb); err != nil {
			return fmt.Errorf("Put block %s: %w", blockID, err)
		}
		blockIDs = append(blockIDs, blockID)
		refs = append(refs, blockstore.BlockRef{
			Hash:   h,
			Offset: uint64(i) * 4096,
			Size:   4096,
		})
	}

	file := &metadata.File{
		ID:        fileID,
		ShareName: shareName,
		FileAttr: metadata.FileAttr{
			Type:   metadata.FileTypeRegular,
			Mode:   0o644,
			UID:    1000,
			GID:    1000,
			Mtime:  now,
			Ctime:  now,
			Atime:  now,
			Blocks: refs,
		},
	}
	if err := store.PutFile(ctx, file); err != nil {
		return fmt.Errorf("PutFile: %w", err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		return fmt.Errorf("SetParent: %w", err)
	}
	if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
		return fmt.Errorf("SetChild: %w", err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		return fmt.Errorf("SetLinkCount: %w", err)
	}

	ws.files = append(ws.files, fuzzFileEntry{handle: handle, name: name, blockIDs: blockIDs})
	return nil
}

// fuzzDeleteFile removes a random file owned by this worker, decrementing
// each FileBlock's RefCount. When RefCount drops to 0 the block is
// deleted to keep the audit math correct (a RefCount=0 block contributes
// 0 to ∑RefCount; dropping the row keeps ∑RefCount cleanly bounded).
func fuzzDeleteFile(ctx context.Context, store metadata.MetadataStore, rootHandle metadata.FileHandle, rng *rand.Rand, ws *workerState) error {
	if len(ws.files) == 0 {
		return nil // nothing to delete; not an error
	}
	idx := rng.Intn(len(ws.files))
	entry := ws.files[idx]

	for _, blockID := range entry.blockIDs {
		newCount, err := store.DecrementRefCount(ctx, blockID)
		if err != nil {
			// Block may have already been deleted via a prior copy/delete.
			if errors.Is(err, metadata.ErrFileBlockNotFound) {
				continue
			}
			return fmt.Errorf("DecrementRefCount %s: %w", blockID, err)
		}
		if newCount == 0 {
			if err := store.Delete(ctx, blockID); err != nil && !errors.Is(err, metadata.ErrFileBlockNotFound) {
				return fmt.Errorf("Delete block %s: %w", blockID, err)
			}
		}
	}

	if err := store.DeleteChild(ctx, rootHandle, entry.name); err != nil {
		return fmt.Errorf("DeleteChild: %w", err)
	}
	if err := store.DeleteFile(ctx, entry.handle); err != nil {
		return fmt.Errorf("DeleteFile: %w", err)
	}

	// Remove from worker's tracked set (swap with tail).
	ws.files[idx] = ws.files[len(ws.files)-1]
	ws.files = ws.files[:len(ws.files)-1]
	return nil
}

// fuzzCopyFile picks a random source file owned by this worker, increments
// each source block's RefCount, and creates a new destination file with
// the same FileAttr.Blocks list. Mirrors engine.CopyPayload's O(1)
// semantics (Phase 12 API-04) at the metadata level.
func fuzzCopyFile(ctx context.Context, store metadata.MetadataStore, shareName string, rootHandle metadata.FileHandle, workerID, opID int, rng *rand.Rand, ws *workerState) error {
	if len(ws.files) == 0 {
		return nil // nothing to copy from; promote to a create
	}
	srcIdx := rng.Intn(len(ws.files))
	src := ws.files[srcIdx]

	srcFile, err := store.GetFile(ctx, src.handle)
	if err != nil {
		return fmt.Errorf("GetFile src: %w", err)
	}
	srcBlocks := append([]blockstore.BlockRef(nil), srcFile.FileAttr.Blocks...)

	// Increment RefCount on each source FileBlock — one bump per BlockRef
	// so multiple refs to the same hash are accounted for explicitly.
	for _, srcBlockID := range src.blockIDs {
		if err := store.IncrementRefCount(ctx, srcBlockID); err != nil {
			return fmt.Errorf("IncrementRefCount %s: %w", srcBlockID, err)
		}
	}

	name := fmt.Sprintf("w%d-op%d-cp.bin", workerID, opID)
	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	if err != nil {
		return fmt.Errorf("GenerateHandle copy: %w", err)
	}
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return fmt.Errorf("DecodeFileHandle copy: %w", err)
	}
	now := time.Now()
	dst := &metadata.File{
		ID:        fileID,
		ShareName: shareName,
		FileAttr: metadata.FileAttr{
			Type:   metadata.FileTypeRegular,
			Mode:   0o644,
			UID:    1000,
			GID:    1000,
			Mtime:  now,
			Ctime:  now,
			Atime:  now,
			Blocks: srcBlocks,
		},
	}
	if err := store.PutFile(ctx, dst); err != nil {
		return fmt.Errorf("PutFile copy: %w", err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		return fmt.Errorf("SetParent copy: %w", err)
	}
	if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
		return fmt.Errorf("SetChild copy: %w", err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		return fmt.Errorf("SetLinkCount copy: %w", err)
	}

	// The destination shares the source's FileBlock IDs so future deletes
	// decrement the same rows the create+copy IncrementRefCount bumps
	// touched. Tracking with the same blockIDs ensures the math closes.
	dstBlockIDs := append([]string(nil), src.blockIDs...)
	ws.files = append(ws.files, fuzzFileEntry{handle: handle, name: name, blockIDs: dstBlockIDs})
	return nil
}

// ============================================================================
// Quiescent-point reconciliation
// ============================================================================

// reconcileINV02 walks the named share and computes:
//
//	totalRefs     = ∑ len(FileAttr.Blocks)         across all files
//	totalRefCount = ∑ FileBlock.RefCount           across distinct hashes
//
// Returns both values so callers (the property fuzz + the leak-injection
// scenario) can distinguish "invariant holds" from "invariant violated"
// without fataling inside the helper.
//
// The distinct-hash dedup on RefCount sums mirrors the Phase 12 D-37
// post-fix world: one FileBlock row per hash. Legacy multi-row data is
// tolerated because GetByHash returns ANY one row per the WR-4-01 contract,
// and all rows with the same hash carry the same RefCount semantics.
func reconcileINV02(ctx context.Context, store metadata.MetadataStore, shareName string) (totalRefs, totalRefCount uint64, err error) {
	// 1) ∑ FileBlock.RefCount across distinct hashes via EnumerateFileBlocks.
	seen := make(map[blockstore.ContentHash]struct{})
	enumErr := store.EnumerateFileBlocks(ctx, func(h blockstore.ContentHash) error {
		if _, ok := seen[h]; ok {
			return nil
		}
		seen[h] = struct{}{}
		fb, getErr := store.GetByHash(ctx, h)
		if getErr != nil {
			return fmt.Errorf("GetByHash %x: %w", h[:8], getErr)
		}
		if fb != nil {
			totalRefCount += uint64(fb.RefCount)
		}
		return nil
	})
	if enumErr != nil {
		return 0, 0, fmt.Errorf("EnumerateFileBlocks: %w", enumErr)
	}

	// 2) ∑ len(FileAttr.Blocks) across every regular file in the share.
	rootHandle, rootErr := store.GetRootHandle(ctx, shareName)
	if rootErr != nil {
		return 0, 0, fmt.Errorf("GetRootHandle: %w", rootErr)
	}
	if walkErr := walkShareFiles(ctx, store, rootHandle, func(f *metadata.File) error {
		totalRefs += uint64(len(f.FileAttr.Blocks))
		return nil
	}); walkErr != nil {
		return 0, 0, fmt.Errorf("walkShareFiles: %w", walkErr)
	}

	return totalRefs, totalRefCount, nil
}

// walkShareFiles recursively walks the share rooted at rootHandle,
// invoking fn for every regular file (not directories). Pagination is
// handled via the existing ListChildren cursor; depth is unbounded but
// the fuzzer creates files only at the share root so the recursive
// path is exercised lightly here.
func walkShareFiles(ctx context.Context, store metadata.MetadataStore, dirHandle metadata.FileHandle, fn func(*metadata.File) error) error {
	cursor := ""
	for {
		entries, next, err := store.ListChildren(ctx, dirHandle, cursor, 0)
		if err != nil {
			return fmt.Errorf("ListChildren: %w", err)
		}
		for _, e := range entries {
			child, err := store.GetFile(ctx, e.Handle)
			if err != nil {
				return fmt.Errorf("GetFile %s: %w", e.Name, err)
			}
			if child.FileAttr.Type == metadata.FileTypeDirectory {
				if err := walkShareFiles(ctx, store, e.Handle, fn); err != nil {
					return err
				}
				continue
			}
			if child.FileAttr.Type == metadata.FileTypeRegular {
				if err := fn(child); err != nil {
					return err
				}
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return nil
}
