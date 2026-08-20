package storetest

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func runBlockRecordOps(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()

	rec := block.BlockRecord{
		BlockID:        "blk-001",
		BlockHash:      block.ContentHash{1, 2, 3},
		Length:         1024,
		LiveChunkCount: 5,
		SyncState:      block.BlockStatePending,
	}

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		if err := store.PutBlockRecord(ctx, rec); err != nil {
			t.Fatalf("PutBlockRecord() error = %v", err)
		}
		got, found, err := store.GetBlockRecord(ctx, rec.BlockID)
		if err != nil {
			t.Fatalf("GetBlockRecord() error = %v", err)
		}
		if !found {
			t.Fatal("GetBlockRecord() found = false, want true")
		}
		if got != rec {
			t.Errorf("GetBlockRecord() = %+v, want %+v", got, rec)
		}
	})

	t.Run("MissingReturnsNotFound", func(t *testing.T) {
		_, found, err := store.GetBlockRecord(ctx, "nonexistent-block")
		if err != nil {
			t.Fatalf("GetBlockRecord(missing) error = %v", err)
		}
		if found {
			t.Fatal("GetBlockRecord(missing) found = true, want false")
		}
	})

	t.Run("Delete", func(t *testing.T) {
		del := block.BlockRecord{
			BlockID:        "blk-del",
			BlockHash:      block.ContentHash{9},
			Length:         512,
			LiveChunkCount: 1,
			SyncState:      block.BlockStatePending,
		}
		if err := store.PutBlockRecord(ctx, del); err != nil {
			t.Fatalf("PutBlockRecord() error = %v", err)
		}
		if err := store.DeleteBlockRecord(ctx, del.BlockID); err != nil {
			t.Fatalf("DeleteBlockRecord() error = %v", err)
		}
		_, found, err := store.GetBlockRecord(ctx, del.BlockID)
		if err != nil {
			t.Fatalf("GetBlockRecord(after delete) error = %v", err)
		}
		if found {
			t.Fatal("GetBlockRecord(after delete) found = true, want false")
		}
	})

	t.Run("Walk", func(t *testing.T) {
		a := block.BlockRecord{BlockID: "walk-a", Length: 100, LiveChunkCount: 1, SyncState: block.BlockStatePending}
		b := block.BlockRecord{BlockID: "walk-b", Length: 200, LiveChunkCount: 2, SyncState: block.BlockStateSyncing}
		if err := store.PutBlockRecord(ctx, a); err != nil {
			t.Fatalf("PutBlockRecord(a) error = %v", err)
		}
		if err := store.PutBlockRecord(ctx, b); err != nil {
			t.Fatalf("PutBlockRecord(b) error = %v", err)
		}

		seen := map[string]bool{}
		if err := store.WalkBlockRecords(ctx, func(r block.BlockRecord) error {
			seen[r.BlockID] = true
			return nil
		}); err != nil {
			t.Fatalf("WalkBlockRecords() error = %v", err)
		}
		if !seen["walk-a"] {
			t.Error("WalkBlockRecords() did not yield walk-a")
		}
		if !seen["walk-b"] {
			t.Error("WalkBlockRecords() did not yield walk-b")
		}
	})

	t.Run("DecrLiveChunkCount", func(t *testing.T) {
		r := block.BlockRecord{BlockID: "decr-test", Length: 256, LiveChunkCount: 10, SyncState: block.BlockStatePending}
		if err := store.PutBlockRecord(ctx, r); err != nil {
			t.Fatalf("PutBlockRecord() error = %v", err)
		}

		// Normal decrement.
		rem, err := store.DecrLiveChunkCount(ctx, r.BlockID, 3)
		if err != nil {
			t.Fatalf("DecrLiveChunkCount() error = %v", err)
		}
		if rem != 7 {
			t.Errorf("DecrLiveChunkCount() remaining = %d, want 7", rem)
		}

		// Floor at zero.
		rem, err = store.DecrLiveChunkCount(ctx, r.BlockID, 100)
		if err != nil {
			t.Fatalf("DecrLiveChunkCount(floor) error = %v", err)
		}
		if rem != 0 {
			t.Errorf("DecrLiveChunkCount(floor) remaining = %d, want 0", rem)
		}
	})

	t.Run("DecrLiveChunkCountMissing", func(t *testing.T) {
		_, err := store.DecrLiveChunkCount(ctx, "does-not-exist", 1)
		if err == nil {
			t.Fatal("DecrLiveChunkCount(missing) expected error, got nil")
		}
	})

	// Concurrent decrements of one block record must every one of them land.
	// DecrLiveChunkCount is a read-modify-write on a single key, so a backend
	// with optimistic concurrency control aborts all but one of a concurrent
	// batch; the store has to retry internally, because the GC caller that
	// drives this has no retry of its own and would silently lose decrements.
	t.Run("ConcurrentDecrLiveChunkCount", func(t *testing.T) {
		const writers = 8
		seed := block.BlockRecord{
			BlockID:        "blk-concurrent-decr",
			BlockHash:      block.ContentHash{9, 9, 9},
			Length:         4096,
			LiveChunkCount: writers,
			SyncState:      block.BlockStatePending,
		}
		if err := store.PutBlockRecord(ctx, seed); err != nil {
			t.Fatalf("PutBlockRecord(seed) error = %v", err)
		}

		errs := make(chan error, writers)
		var wg sync.WaitGroup
		for range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := store.DecrLiveChunkCount(ctx, seed.BlockID, 1)
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("concurrent DecrLiveChunkCount() error = %v", err)
			}
		}

		got, found, err := store.GetBlockRecord(ctx, seed.BlockID)
		if err != nil || !found {
			t.Fatalf("GetBlockRecord() after concurrent decrements: found = %v, err = %v", found, err)
		}
		if got.LiveChunkCount != 0 {
			t.Errorf("LiveChunkCount = %d after %d concurrent decrements of 1, want 0 (a lost decrement means a retry was skipped)", got.LiveChunkCount, writers)
		}
	})
}
