package badger_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// openRelaxed opens a relaxed-durability badger store at dbPath. Reusable so a
// reopen test can point two store instances at the same directory.
func openRelaxed(t *testing.T, dbPath string) *badger.BadgerMetadataStore {
	t.Helper()
	// WithDefaultsAndCaches applies the default filesystem capabilities the
	// conformance suite expects, plus the relaxed-durability flag.
	store, err := badger.NewBadgerMetadataStoreWithDefaultsAndCaches(context.Background(), dbPath, 0, 0, true)
	require.NoError(t, err)
	return store
}

func openStrict(t *testing.T, dbPath string) *badger.BadgerMetadataStore {
	t.Helper()
	store, err := badger.NewBadgerMetadataStoreWithDefaultsAndCaches(context.Background(), dbPath, 0, 0, false)
	require.NoError(t, err)
	return store
}

// TestConformance_RelaxedDurability runs the entire metadata conformance suite
// against a relaxed-durability store (#1573 Wall 1). It is the primary
// no-loss/no-corruption guard: every create/write/rename/remove/truncate/link/
// xattr/attr operation the suite exercises must still read back correctly when
// namespace-op fsyncs are deferred. A regression that dropped or corrupted data
// on the relaxed path fails here exactly as it would on the strict store.
func TestConformance_RelaxedDurability(t *testing.T) {
	storetest.RunConformanceSuite(t, func(t *testing.T) metadata.Store {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store := openRelaxed(t, dbPath)
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("store.Close() failed: %v", err)
			}
		})
		return store
	})
}

// TestRelaxedDurability_DurablePathFsyncsInline pins the durable/relaxed
// classification: in relaxed mode a durable commit (WithTransaction) fsyncs
// inline, while a relaxed commit (WithTransactionRelaxed) does not. If a future
// change accidentally routed a durable write through the relaxed path
// (reintroducing the #588 silent-zeros hazard), the inline-sync count would not
// advance and this fails.
func TestRelaxedDurability_DurablePathFsyncsInline(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store := openRelaxed(t, dbPath)
	t.Cleanup(func() { _ = store.Close() })

	// Durable commit must fsync inline.
	before := store.InlineSyncCountForTest()
	require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error { return nil }))
	require.Equal(t, before+1, store.InlineSyncCountForTest(),
		"durable WithTransaction must fsync inline in relaxed mode")

	// Relaxed commit must NOT fsync inline (bounded-lag syncer handles it).
	before = store.InlineSyncCountForTest()
	require.NoError(t, store.WithTransactionRelaxed(ctx, func(tx metadata.Transaction) error { return nil }))
	require.Equal(t, before, store.InlineSyncCountForTest(),
		"relaxed WithTransactionRelaxed must NOT fsync inline")
}

// TestRelaxedDurability_StrictModeNoInlineSync verifies that with relaxed
// durability disabled (the opt-out), no inline db.Sync() runs — durability is
// carried by SyncWrites=true on every commit, exactly reproducing the pre-#1573
// posture.
func TestRelaxedDurability_StrictModeNoInlineSync(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store := openStrict(t, dbPath)
	t.Cleanup(func() { _ = store.Close() })

	before := store.InlineSyncCountForTest()
	require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error { return nil }))
	require.NoError(t, store.WithTransactionRelaxed(ctx, func(tx metadata.Transaction) error { return nil }))
	require.Equal(t, before, store.InlineSyncCountForTest(),
		"strict mode must fsync via SyncWrites, never inline db.Sync")
}

// TestRelaxedDurability_FlushPendingWriteRouting pins the #1687 routing: the
// metadata Service's FlushPendingWriteForFile must route its size/mtime commit to
// WithTransactionRelaxed when durable=false (no inline fsync — the deferred hot
// path) and to WithTransaction when durable=true (inline fsync — FILE_SYNC /
// CLOSE / FLUSH / shutdown). If a future change inverted or dropped this routing,
// the inline-sync count deltas below flip and this fails.
func TestRelaxedDurability_FlushPendingWriteRouting(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store := openRelaxed(t, dbPath)
	t.Cleanup(func() { _ = store.Close() })

	const shareName = "/test"
	root, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(shareName, root.ID)
	require.NoError(t, err)

	svc := metadata.New() // deferred commit is on by default
	require.NoError(t, svc.RegisterStoreForShare(shareName, store))

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
	}

	// stagePending creates a file and records a deferred (pending) size write, so
	// FlushPendingWriteForFile has real work to commit.
	stagePending := func(name string, size uint64) metadata.FileHandle {
		_, _, cerr := svc.CreateFile(authCtx, rootHandle, name, &metadata.FileAttr{Mode: 0o644})
		require.NoError(t, cerr)
		h, gerr := store.GetChild(ctx, rootHandle, name)
		require.NoError(t, gerr)
		intent, perr := svc.PrepareWrite(authCtx, h, size)
		require.NoError(t, perr)
		_, cwErr := svc.CommitWrite(authCtx, intent)
		require.NoError(t, cwErr)
		return h
	}

	// durable=false → relaxed commit, NO inline fsync.
	hRelaxed := stagePending("relaxed.bin", 4096)
	before := store.InlineSyncCountForTest()
	flushed, ferr := svc.FlushPendingWriteForFile(authCtx, hRelaxed, false)
	require.NoError(t, ferr)
	require.True(t, flushed, "relaxed flush must have pending size to commit")
	require.Equal(t, before, store.InlineSyncCountForTest(),
		"durable=false must route through WithTransactionRelaxed (no inline fsync)")

	// durable=true → strict commit, exactly one inline fsync.
	hDurable := stagePending("durable.bin", 4096)
	before = store.InlineSyncCountForTest()
	flushed, ferr = svc.FlushPendingWriteForFile(authCtx, hDurable, true)
	require.NoError(t, ferr)
	require.True(t, flushed, "durable flush must have pending size to commit")
	require.Equal(t, before+1, store.InlineSyncCountForTest(),
		"durable=true must route through WithTransaction (inline fsync)")
}

// TestRelaxedDurability_DurableWriteSurvivesReopen confirms a durable write on
// the relaxed store persists across a clean close+reopen with no loss — the
// everyday durability the deferred-fsync path must never compromise.
func TestRelaxedDurability_DurableWriteSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")

	rec := block.BlockRecord{BlockID: "blk-A", Length: 8192, LiveChunkCount: 1, SyncState: block.BlockStateRemote}

	store := openRelaxed(t, dbPath)
	require.NoError(t, store.PutBlockRecord(ctx, rec)) // durable write path
	require.NoError(t, store.Close())                  // clean close flushes everything durably

	reopened := openRelaxed(t, dbPath)
	t.Cleanup(func() { _ = reopened.Close() })
	got, found, err := reopened.GetBlockRecord(ctx, "blk-A")
	require.NoError(t, err)
	require.True(t, found, "durable block record must survive close+reopen")
	require.Equal(t, rec, got, "durable block record must survive close+reopen")
}
