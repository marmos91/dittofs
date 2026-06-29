package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// stubHold is a HoldProvider that injects a fixed set of held hashes into the
// mark phase, standing in for snapshot holds.
type stubHold struct {
	held []block.ContentHash
}

func (s stubHold) HeldHashes(_ context.Context, _ string, _ []string, fn func(block.ContentHash) error) error {
	for _, h := range s.held {
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// TestCollectGarbageLocal_SweepsOrphansKeepsLive proves the local-tier sweep
// reclaims orphaned chunks while preserving live ones — the same mark-sweep
// kernel as the remote pass, driven through CollectGarbageLocal against a
// generic block.Store (#1433). IsLocalTier must be tagged.
func TestCollectGarbageLocal_SweepsOrphansKeepsLive(t *testing.T) {
	ctx := t.Context()
	// A memory remote store satisfies block.Store (Walk+Delete+Put), standing
	// in for a share's local CAS namespace.
	ls := remotememory.New()
	defer func() { _ = ls.Close() }()
	old := time.Now().Add(-2 * time.Hour)
	ls.SetNowFnForTest(func() time.Time { return old })

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	live := []block.ContentHash{hashFromString("L1"), hashFromString("L2")}
	orphans := []block.ContentHash{hashFromString("O1"), hashFromString("O2")}
	for i, h := range live {
		putBlock(t, st, fmt.Sprintf("file-live/%d", i), h)
		if err := ls.Put(ctx, h, []byte("live-"+string(rune('a'+i)))); err != nil {
			t.Fatalf("Put live: %v", err)
		}
	}
	for i, h := range orphans {
		if err := ls.Put(ctx, h, []byte("orphan-"+string(rune('a'+i)))); err != nil {
			t.Fatalf("Put orphan: %v", err)
		}
	}

	stats := CollectGarbageLocal(ctx, ls, rec, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Minute,
		Shares:      []string{"share-a"},
	})

	if !stats.IsLocalTier {
		t.Error("IsLocalTier = false, want true")
	}
	if stats.HashesMarked != 2 {
		t.Errorf("HashesMarked = %d, want 2", stats.HashesMarked)
	}
	if stats.ObjectsSwept != 2 {
		t.Errorf("ObjectsSwept = %d, want 2 (orphans)", stats.ObjectsSwept)
	}
	if stats.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d, want 0; %v", stats.ErrorCount, stats.FirstErrors)
	}
	for _, h := range live {
		if _, err := ls.Get(ctx, h); err != nil {
			t.Errorf("live chunk %x swept: %v", h[:8], err)
		}
	}
	for _, h := range orphans {
		if _, err := ls.Get(ctx, h); err == nil {
			t.Errorf("orphan %x not swept", h[:8])
		}
	}
}

// TestCollectGarbageLocal_SkipsSnapshotHeldChunks proves the local sweep never
// deletes a chunk held by a snapshot, even when no live file references it —
// the must-not-break-snapshots guardrail (#1433).
func TestCollectGarbageLocal_SkipsSnapshotHeldChunks(t *testing.T) {
	ctx := t.Context()
	ls := remotememory.New()
	defer func() { _ = ls.Close() }()
	old := time.Now().Add(-2 * time.Hour)
	ls.SetNowFnForTest(func() time.Time { return old })

	rec := newGCMSReconciler()
	rec.addShare("share-a") // no live files

	held := hashFromString("held-by-snapshot")
	orphan := hashFromString("real-orphan")
	if err := ls.Put(ctx, held, []byte("snapshot-data")); err != nil {
		t.Fatalf("Put held: %v", err)
	}
	if err := ls.Put(ctx, orphan, []byte("orphan-data")); err != nil {
		t.Fatalf("Put orphan: %v", err)
	}

	stats := CollectGarbageLocal(ctx, ls, rec, &Options{
		GCStateRoot:  t.TempDir(),
		GracePeriod:  time.Minute,
		Shares:       []string{"share-a"},
		HoldProvider: stubHold{held: []block.ContentHash{held}},
	})

	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, want 0; %v", stats.ErrorCount, stats.FirstErrors)
	}
	if _, err := ls.Get(ctx, held); err != nil {
		t.Errorf("snapshot-held chunk %x was swept — snapshot data loss", held[:8])
	}
	if _, err := ls.Get(ctx, orphan); err == nil {
		t.Errorf("orphan %x not swept", orphan[:8])
	}
	if stats.ObjectsSwept != 1 {
		t.Errorf("ObjectsSwept = %d, want 1 (only the true orphan)", stats.ObjectsSwept)
	}
}
