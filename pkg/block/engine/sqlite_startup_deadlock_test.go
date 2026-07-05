package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// These tests exercise the three EnumerateSynced-then-GetLocator sites
// (Store.Start's legacy-CAS migration, the Reconcile reporter, and
// ReclaimRecords) against a REAL sqlite metadata store. The sqlite backend caps
// its pool at SetMaxOpenConns(1), so issuing GetLocator from inside an
// EnumerateSynced callback — while the rows cursor still holds the only
// connection — deadlocks forever (#1552/#1553). The entire e2e/integration suite
// runs on the memory metadata store, which has no connection cap, so this whole
// class was invisible to CI and only surfaced on a live sqlite+S3 VM.
//
// Every case runs under a bounded timeout so a regression of the collect-then-
// query fix surfaces as a test FAILURE, never a hung run.

// sqliteDeadlockTimeout bounds each guarded operation. On fixed code the
// operations complete in milliseconds; a nested-query deadlock cannot resolve,
// so it hits this deadline (the sqlite pool honours ctx while waiting for a
// connection) and the run below fails deterministically instead of hanging.
const sqliteDeadlockTimeout = 5 * time.Second

// runBounded runs fn under a bounded context and a watchdog. It fails the test
// if fn returns an error (the sqlite pool returns ctx.DeadlineExceeded when a
// nested query waits forever for the single connection) or does not return at
// all within a slightly longer watchdog window (a true hang that ignores ctx).
func runBounded(t *testing.T, name string, fn func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sqliteDeadlockTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s failed — nested-query-inside-enumeration deadlock on sqlite "+
				"MaxOpenConns(1)? (regression of #1552/#1553): %v", name, err)
		}
	case <-time.After(sqliteDeadlockTimeout + 5*time.Second):
		t.Fatalf("%s did not return — sqlite nested-query-inside-enumeration deadlock "+
			"(regression of #1552/#1553)", name)
	}
}

// cleanupClose registers a bounded Close in t.Cleanup. If a regression leaves a
// goroutine wedged on the single sqlite connection (the true-hang path guarded
// by runBounded's watchdog), a synchronous Close could block on it and turn a
// fast failure back into a hung run — so cleanup waits only briefly, then moves
// on. On the normal (passing) path Close returns immediately.
func cleanupClose(t *testing.T, name string, closeFn func() error) {
	t.Helper()
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { _ = closeFn(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Logf("%s: Close did not return within 5s (a wedged goroutine still holds the connection)", name)
		}
	})
}

// newSQLiteStoreForDeadlockTest builds a fresh, migrated on-disk sqlite store.
func newSQLiteStoreForDeadlockTest(t *testing.T) *sqlite.SQLiteMetadataStore {
	t.Helper()
	cfg := &sqlite.SQLiteMetadataStoreConfig{
		Path:        filepath.Join(t.TempDir(), "meta.db"),
		AutoMigrate: true,
	}
	st, err := sqlite.NewSQLiteMetadataStore(context.Background(), cfg, metadata.FilesystemCapabilities{
		MaxFilenameLen:      255,
		MaxPathLen:          4096,
		TimestampResolution: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("NewSQLiteMetadataStore: %v", err)
	}
	cleanupClose(t, "sqlite store", st.Close)
	return st
}

// seedSyncedMarkers marks n block-resident synced hashes (BlockID != ""), enough
// for EnumerateSynced to yield rows so the nested GetLocator actually runs.
// Returns the hashes in insertion order.
func seedSyncedMarkers(t *testing.T, st *sqlite.SQLiteMetadataStore, blockID string, n int) []block.ContentHash {
	t.Helper()
	ctx := context.Background()
	hashes := make([]block.ContentHash, 0, n)
	for i := 0; i < n; i++ {
		h := hashFromString(fmt.Sprintf("%s-chunk-%d", blockID, i))
		if err := st.MarkSynced(ctx, h, block.ChunkLocator{BlockID: blockID, WireOffset: int64(i * 16), WireLength: 16}); err != nil {
			t.Fatalf("MarkSynced: %v", err)
		}
		hashes = append(hashes, h)
	}
	return hashes
}

// TestSQLiteStartup_LegacyCASMigration_NoDeadlock drives the real block engine's
// Store.Start against a sqlite-backed SyncedHashStore. Start runs the one-shot
// legacy-CAS migration (migrateLegacyCASRemote) synchronously — the exact path
// that blocked whole-server startup in #1552 — which enumerates synced hashes
// and resolves each locator. With block-resident markers nothing is standalone,
// so no repack happens, but the deadlock-prone enumerate+GetLocator pass still
// runs against the single-connection pool.
func TestSQLiteStartup_LegacyCASMigration_NoDeadlock(t *testing.T) {
	st := newSQLiteStoreForDeadlockTest(t)
	seedSyncedMarkers(t, st, "blk-live", 8)

	local := localmemory.New()
	rbs := remotememory.New()
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(local, rbs, fbs, DefaultConfig())
	syncer.SetRemoteBlockStore(rbs)

	bs, err := New(BlockStoreConfig{
		Local:           local,
		Remote:          rbs,
		Syncer:          syncer,
		FileChunkStore:  fbs,
		SyncedHashStore: st,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cleanupClose(t, "block store", bs.Close)

	runBounded(t, "Store.Start (legacy-CAS migration)", bs.Start)
}

// TestSQLiteReconcile_NoDeadlock runs the read-only Reconcile reporter against a
// real sqlite view. Reconcile enumerates synced hashes then resolves every
// locator; on the single-connection pool a nested resolve would deadlock.
func TestSQLiteReconcile_NoDeadlock(t *testing.T) {
	st := newSQLiteStoreForDeadlockTest(t)
	ctx := context.Background()
	seedSyncedMarkers(t, st, "blk-live", 8)

	// A healthy record (still referenced by a live locator) and an orphan record
	// (zero-ref, no live locator) so the reporter classifies real data, not just
	// an empty scan.
	if err := st.PutBlockRecord(ctx, block.BlockRecord{BlockID: "blk-live", Length: 128, LiveChunkCount: 8, SyncState: block.BlockStateRemote}); err != nil {
		t.Fatalf("PutBlockRecord(live): %v", err)
	}
	if err := st.PutBlockRecord(ctx, block.BlockRecord{BlockID: "blk-orphan", Length: 64, LiveChunkCount: 0, SyncState: block.BlockStateRemote}); err != nil {
		t.Fatalf("PutBlockRecord(orphan): %v", err)
	}

	var rep ReconcileReport
	runBounded(t, "Reconcile", func(ctx context.Context) error {
		var err error
		rep, err = Reconcile(ctx, []ReconcileMetaView{st}, nil, nil, ReconcileOptions{})
		return err
	})

	if rep.ZeroRefRecords.Count != 1 || (len(rep.ZeroRefRecords.Sample) > 0 && rep.ZeroRefRecords.Sample[0] != "blk-orphan") {
		t.Errorf("ZeroRefRecords = %+v; want count 1 sample [blk-orphan]", rep.ZeroRefRecords)
	}
	if rep.LeakedBlocks.Count != 0 {
		t.Errorf("LeakedBlocks.Count = %d; want 0 (blk-live is still referenced)", rep.LeakedBlocks.Count)
	}
}

// TestSQLiteReclaimRecords_NoDeadlock runs the (dry-run) record reclaimer against
// a real sqlite view. ReclaimRecords walks block records, enumerates synced
// hashes, then resolves every locator to drop still-referenced blocks from the
// candidate set — the same single-connection nested-query hazard.
func TestSQLiteReclaimRecords_NoDeadlock(t *testing.T) {
	st := newSQLiteStoreForDeadlockTest(t)
	ctx := context.Background()
	seedSyncedMarkers(t, st, "blk-live", 8)

	if err := st.PutBlockRecord(ctx, block.BlockRecord{BlockID: "blk-live", Length: 128, LiveChunkCount: 8, SyncState: block.BlockStateRemote}); err != nil {
		t.Fatalf("PutBlockRecord(live): %v", err)
	}
	if err := st.PutBlockRecord(ctx, block.BlockRecord{BlockID: "blk-orphan", Length: 64, LiveChunkCount: 0, SyncState: block.BlockStateRemote}); err != nil {
		t.Fatalf("PutBlockRecord(orphan): %v", err)
	}

	var rep ReclaimReport
	runBounded(t, "ReclaimRecords", func(ctx context.Context) error {
		var err error
		rep, err = ReclaimRecords(ctx, []ReclaimMetaView{st}, nil, ReclaimOptions{DryRun: true})
		return err
	})

	if rep.Reclaimed.Count != 1 || (len(rep.Reclaimed.Sample) > 0 && rep.Reclaimed.Sample[0] != "blk-orphan") {
		t.Errorf("Reclaimed = %+v; want count 1 sample [blk-orphan]", rep.Reclaimed)
	}
}

// countingMetaView wraps a real sqlite view and counts the query round-trips the
// collect-then-query pass issues, so a test can observe the N+1 shape.
type countingMetaView struct {
	*sqlite.SQLiteMetadataStore
	enumerateCalls int
	getLocatorCall int
}

func (v *countingMetaView) EnumerateSynced(ctx context.Context, fn func(block.ContentHash, time.Time) error) error {
	v.enumerateCalls++
	return v.SQLiteMetadataStore.EnumerateSynced(ctx, fn)
}

func (v *countingMetaView) GetLocator(ctx context.Context, h block.ContentHash) (block.ChunkLocator, bool, error) {
	v.getLocatorCall++
	return v.SQLiteMetadataStore.GetLocator(ctx, h)
}

// TestSQLiteReconcile_LocatorRoundTripsAreLinear documents the slow-startup
// finding of #1554. After the collect-then-query deadlock fix, the migration /
// reconcile / reclaim passes are correct but still issue one GetLocator round
// trip per synced hash. On the sqlite backend every one of those round trips is
// serialized on the single pooled connection (SetMaxOpenConns(1)), so the cost
// of resolving locators is O(synced-hash-count) sequential statements — the
// latent serialization behind the ">120s to first healthy" symptom on a large
// sqlite+S3 share. This test pins that N+1 shape so the profiling finding is
// observable in CI; the batch fix is to fold the locator columns into the
// EnumerateSynced scan (they already live in the same synced_hashes row).
func TestSQLiteReconcile_LocatorRoundTripsAreLinear(t *testing.T) {
	st := newSQLiteStoreForDeadlockTest(t)
	const n = 32
	seedSyncedMarkers(t, st, "blk-live", n)

	view := &countingMetaView{SQLiteMetadataStore: st}
	if _, err := Reconcile(context.Background(), []ReconcileMetaView{view}, nil, nil, ReconcileOptions{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if view.enumerateCalls != 1 {
		t.Errorf("EnumerateSynced calls = %d; want 1", view.enumerateCalls)
	}
	// One serial GetLocator per synced hash: the O(n) round-trip pattern that
	// serializes on the single sqlite connection at startup.
	if view.getLocatorCall != n {
		t.Errorf("GetLocator round trips = %d; want %d (one per synced hash)", view.getLocatorCall, n)
	}
	t.Logf("sqlite locator resolution issued %d serial GetLocator round trips for %d synced hashes "+
		"(each serialized on the MaxOpenConns(1) pool) — see #1554 profiling finding", view.getLocatorCall, n)
}
