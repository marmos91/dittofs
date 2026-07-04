package engine

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestGCIndexSweep_DeletesOrphansWithoutWalk proves the index-based remote
// sweep: it reclaims a past-grace orphan present in the synced index but
// absent from the live set — through its packed block, never via a remote
// LIST — while preserving (a) a live chunk, (b) a within-grace freshly-synced
// chunk, and (c) a legacy marker with no recorded timestamp (fail-closed).
// The swept orphan's marker is cleared so it re-uploads if it reappears live
// (#1433).
func TestGCIndexSweep_DeletesOrphansWithoutWalk(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	live := hashFromString("lf-live-keep")      // in manifest → live
	orphan := hashFromString("lf-orphan-sweep") // not in manifest, past grace → swept
	fresh := hashFromString("lf-fresh-keep")    // not in manifest, within grace → kept
	legacy := hashFromString("lf-legacy-keep")  // not in manifest, zero ts → fail-closed keep

	// Only the live chunk has a manifest row (FileChunk).
	putBlock(t, st, "file-live/0", live)
	for _, h := range []block.ContentHash{live, orphan, fresh, legacy} {
		seedRemoteChunk(t, st, rs, h) // marker backdated past grace
	}
	mm := st.(*metadatamemory.MemoryMetadataStore)
	// fresh marked just now — within the grace window.
	mm.MarkSyncedAtForTest(fresh, time.Now())
	// legacy marker carries no timestamp (pre-upgrade badger marker).
	mm.MarkSyncedAtForTest(legacy, time.Time{})

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Hour,
	})

	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, want 0 (FirstErrors=%v)", stats.ErrorCount, stats.FirstErrors)
	}
	if stats.ObjectsSwept != 1 {
		t.Fatalf("ObjectsSwept = %d, want 1 (only the past-grace orphan)", stats.ObjectsSwept)
	}

	// Orphan: block reclaimed, marker cleared.
	if chunkOnRemote(t, st, orphan) {
		t.Errorf("orphan still remote-reachable after index sweep")
	}
	if ok, _ := st.IsSynced(ctx, orphan); ok {
		t.Errorf("swept orphan still marked synced; ListUnsynced would skip it forever (#1433)")
	}

	// Live, fresh, legacy: kept with markers intact.
	for name, h := range map[string]block.ContentHash{"live": live, "fresh": fresh, "legacy": legacy} {
		if !chunkOnRemote(t, st, h) {
			t.Errorf("%s chunk wrongly reclaimed", name)
		}
		if ok, _ := st.IsSynced(ctx, h); !ok {
			t.Errorf("%s chunk's synced marker wrongly cleared", name)
		}
	}
}

// TestGCIndexSweep_DryRunCountsWithoutDeleting verifies dry-run reports the
// orphan as a candidate without reclaiming it or clearing its marker.
func TestGCIndexSweep_DryRunCountsWithoutDeleting(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	orphan := hashFromString("lf-dryrun-orphan")
	seedRemoteChunk(t, st, rs, orphan)

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Hour,
		DryRun:      true,
	})

	if stats.ObjectsSwept != 1 {
		t.Fatalf("dry-run ObjectsSwept = %d, want 1 (candidate count)", stats.ObjectsSwept)
	}
	if !chunkOnRemote(t, st, orphan) {
		t.Errorf("dry-run reclaimed the orphan")
	}
	if ok, _ := st.IsSynced(ctx, orphan); !ok {
		t.Errorf("dry-run cleared the synced marker")
	}
}

// TestGCIndexSweep_NoReclaimerFailsClosed proves the post-#1493 drift
// handling: with no BlockReclaimer wired the sweep must record the condition
// and keep both the marker and the dead chunk — it never guesses at a remote
// key to delete.
func TestGCIndexSweep_NoReclaimerFailsClosed(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	orphan := hashFromString("lf-noreclaimer-orphan")
	seedRemoteChunk(t, st, rs, orphan)

	idx, ok := st.(SyncedHashIndex)
	if !ok {
		t.Fatalf("metadata store %T does not implement SyncedHashIndex", st)
	}
	stats := CollectGarbage(ctx, rec, &Options{
		GCStateRoot:     t.TempDir(),
		GracePeriod:     time.Minute,
		SyncedHashIndex: idx,
		// BlockReclaimer deliberately nil.
	})

	if stats.ErrorCount == 0 {
		t.Fatalf("ErrorCount = 0, want > 0 (missing reclaimer is drift)")
	}
	if stats.ObjectsSwept != 0 {
		t.Errorf("ObjectsSwept = %d, want 0 (nothing reclaimable without a reclaimer)", stats.ObjectsSwept)
	}
	if ok, _ := st.IsSynced(ctx, orphan); !ok {
		t.Errorf("marker cleared despite fail-closed skip")
	}
	if !chunkOnRemote(t, st, orphan) {
		t.Errorf("chunk reclaimed despite missing reclaimer")
	}
}
