package engine_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestBadgerConcurrentIdenticalContent_Converges drives N concurrent rollups
// of BYTE-IDENTICAL content through the real engine over the badger metadata
// backend and asserts ALL converge: a concurrent Flush of every file's
// identical content provokes badger SSI optimistic-concurrency aborts on the
// hot dedup keys (the obj:<hash> object_id index + the content-hash FileBlock
// rows), exactly the bulk-same-content (zero/padding tar block) pattern from
// #1245-B.
//
// Before the fix, badger's WithTransaction leaked the raw badgerdb.ErrConflict
// sentinel on the SSI hot-key path; the rollup persister's conflict handler
// could not recognize it as an object_id conflict, so the duplicate's blocks
// never fell back to the zero-objectID write and the Flush surfaced the raw
// conflict (and the background ticker would re-run the same payloadID forever).
// After the fix, every file converges:
//   - Flush succeeds for all files,
//   - every file has a complete, restorable block list,
//   - exactly one file owns the canonical object_id (the rest are zero).
func TestBadgerConcurrentIdenticalContent_Converges(t *testing.T) {
	// Shrink the badger SSI retry budget to 1 so any overlapping write-write on
	// the hot dedup keys surfaces an exhausted-retry conflict immediately
	// (instead of being absorbed by the default 20-retry backoff). This makes
	// the #1245-B livelock path deterministic: the rollup persister MUST
	// recognize the (now-wrapped) conflict and converge to the zero-objectID
	// write for every duplicate rather than bubbling the raw conflict back to
	// the drain/ticker.
	restore := metadatabadger.SetMaxTransactionRetriesForTest(1)
	t.Cleanup(restore)

	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })

	ctx := context.Background()
	shareName := "badger-conc-dup"
	rootHandle := createShare(t, ms, shareName)
	bs := newEngineOverStore(t, ms)

	const nFiles = 12
	// Byte-identical 3MB content across every file → all share the same
	// Merkle-root object_id and the same per-chunk content hashes → maximal
	// dedup-key contention under concurrent rollup.
	content := distinctContent(0x42, 3*1024*1024)

	pids := make([]string, nFiles)
	handles := make([]metadata.FileHandle, nFiles)
	for i := 0; i < nFiles; i++ {
		pid, h := createRealFile(t, ms, shareName, fmt.Sprintf("dup-%02d.bin", i), rootHandle)
		pids[i] = pid
		handles[i] = h
		if _, werr := bs.WriteAt(ctx, pid, nil, content, 0); werr != nil {
			t.Fatalf("WriteAt file %d: %v", i, werr)
		}
	}

	// Concurrently drive rollup-to-CAS+manifest from many goroutines. Each
	// DrainRollups iterates the whole dirty set; the per-file mutex inside
	// rollupFile serializes same-file passes but lets DIFFERENT identical-
	// content files roll up in parallel, so their ObjectIDPersister calls
	// concurrently contend on the same hot badger dedup keys (the obj:<hash>
	// object_id index + the content-hash FileBlock rows) → SSI conflicts.
	const drainers = 8
	errs := make([]error, drainers)
	var wg sync.WaitGroup
	wg.Add(drainers)
	for d := 0; d < drainers; d++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = bs.DrainRollups(ctx)
		}(d)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent DrainRollups %d must converge (no looping/leaked conflict): %v", i, e)
		}
	}

	// A final serial DrainRollups must also be clean (no residual conflict
	// wedged, all dirty intervals fully drained).
	if derr := bs.DrainRollups(ctx); derr != nil {
		t.Fatalf("final DrainRollups must converge: %v", derr)
	}

	// Every file must have a complete, restorable block list; exactly one owns
	// the canonical object_id.
	owners := 0
	want := block.NewHashSet(0)
	for i := 0; i < nFiles; i++ {
		f, gerr := ms.GetFile(ctx, handles[i])
		if gerr != nil {
			t.Fatalf("GetFile file %d: %v", i, gerr)
		}
		if len(f.Blocks) == 0 {
			t.Fatalf("file %d has empty FileAttr.Blocks after convergence (unrestorable)", i)
		}
		for _, b := range f.Blocks {
			want.Add(b.Hash)
		}
		if !f.ObjectID.IsZero() {
			owners++
		}
	}

	if owners != 1 {
		t.Fatalf("expected exactly one object_id owner, got %d", owners)
	}

	// The Backup manifest must reference every block hash (all files
	// restorable).
	got, bufLen := manifestHashes(t, ms)
	missing := 0
	for _, h := range want.Sorted() {
		if !got.Contains(h) {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("Backup manifest missing %d/%d referenced hashes (manifest len=%d, buf=%d bytes)",
			missing, want.Len(), got.Len(), bufLen)
	}
}
