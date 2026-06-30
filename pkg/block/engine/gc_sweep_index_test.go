package engine

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// noWalkRemote fails the test if RemoteStore.Walk is called. The LIST-free
// sweep must derive its candidates from the synced-hash index alone, never
// from an S3 LIST — wrapping the remote this way turns that invariant into a
// hard assertion.
type noWalkRemote struct {
	remote.RemoteStore
	t *testing.T
}

func (n noWalkRemote) Walk(context.Context, func(block.ContentHash, block.Meta) error) error {
	n.t.Fatalf("LIST-free sweep must not call RemoteStore.Walk (S3 LIST)")
	return nil
}

// TestGCIndexSweep_DeletesOrphansWithoutWalk proves the index-based remote
// sweep (steady-state, FullScan false): it reclaims a past-grace orphan present
// in the synced index but absent from the live set, WITHOUT walking the remote,
// while preserving (a) a live block, (b) a within-grace freshly-synced block,
// and (c) a legacy marker with no recorded timestamp (fail-closed). The swept
// orphan's marker is cleared so it re-uploads if it reappears live (#1433).
func TestGCIndexSweep_DeletesOrphansWithoutWalk(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	now := time.Now()
	live := hashFromString("lf-live-keep")      // in manifest → live
	orphan := hashFromString("lf-orphan-sweep") // not in manifest, past grace → swept
	fresh := hashFromString("lf-fresh-keep")    // not in manifest, within grace → kept
	legacy := hashFromString("lf-legacy-keep")  // not in manifest, zero ts → fail-closed keep

	// Only the live block has a manifest row (FileChunk).
	putBlock(t, st, "file-live/0", live)
	for _, h := range []block.ContentHash{live, orphan, fresh, legacy} {
		writeCASObject(t, ctx, rs, h, []byte("data-"+h.String()[:8]))
	}

	synced := newRecordingSyncedHashStore()
	// live & orphan marked LONG ago (past grace) — only the live-set check can
	// save the live one; grace cannot.
	synced.markSyncedAtForTest(live, now.Add(-2*time.Hour))
	synced.markSyncedAtForTest(orphan, now.Add(-2*time.Hour))
	// fresh marked just now — within the grace window.
	synced.markSyncedAtForTest(fresh, now)
	// legacy marker carries no timestamp (pre-upgrade badger marker).
	synced.markSyncedAtForTest(legacy, time.Time{})

	stats := CollectGarbage(ctx, noWalkRemote{RemoteStore: rs, t: t}, rec, &Options{
		GCStateRoot:     t.TempDir(),
		GracePeriod:     time.Hour,
		SyncedHashIndex: synced,
		// FullScan omitted (false) → steady-state index sweep.
	})

	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, want 0 (FirstErrors=%v)", stats.ErrorCount, stats.FirstErrors)
	}
	if stats.ObjectsSwept != 1 {
		t.Fatalf("ObjectsSwept = %d, want 1 (only the past-grace orphan)", stats.ObjectsSwept)
	}

	// Orphan: deleted from remote, marker cleared.
	if _, err := rs.Get(ctx, orphan); err == nil {
		t.Errorf("orphan still present on remote after LIST-free sweep")
	}
	if ok, _ := synced.IsSynced(ctx, orphan); ok {
		t.Errorf("swept orphan still marked synced; ListUnsynced would skip it forever (#1433)")
	}

	// Live, fresh, legacy: kept on remote with markers intact.
	for name, h := range map[string]block.ContentHash{"live": live, "fresh": fresh, "legacy": legacy} {
		if _, err := rs.Get(ctx, h); err != nil {
			t.Errorf("%s block wrongly deleted: %v", name, err)
		}
		if ok, _ := synced.IsSynced(ctx, h); !ok {
			t.Errorf("%s block's synced marker wrongly cleared", name)
		}
	}
}

// TestGCIndexSweep_DryRunCountsWithoutDeleting verifies dry-run reports the
// orphan as a candidate without deleting it or clearing its marker.
func TestGCIndexSweep_DryRunCountsWithoutDeleting(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	_ = rec.addShare("share-a")

	orphan := hashFromString("lf-dryrun-orphan")
	writeCASObject(t, ctx, rs, orphan, []byte("orphan-data"))

	synced := newRecordingSyncedHashStore()
	synced.markSyncedAtForTest(orphan, time.Now().Add(-2*time.Hour))

	stats := CollectGarbage(ctx, noWalkRemote{RemoteStore: rs, t: t}, rec, &Options{
		GCStateRoot:     t.TempDir(),
		GracePeriod:     time.Hour,
		SyncedHashIndex: synced,
		DryRun:          true,
		// FullScan omitted (false) → steady-state index sweep.
	})

	if stats.ObjectsSwept != 1 {
		t.Fatalf("dry-run ObjectsSwept = %d, want 1 (candidate count)", stats.ObjectsSwept)
	}
	if _, err := rs.Get(ctx, orphan); err != nil {
		t.Errorf("dry-run deleted the orphan: %v", err)
	}
	if ok, _ := synced.IsSynced(ctx, orphan); !ok {
		t.Errorf("dry-run cleared the synced marker")
	}
}
