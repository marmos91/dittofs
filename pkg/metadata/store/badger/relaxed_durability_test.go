package badger_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

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
// classification: in relaxed mode a durable commit (WithTransaction) and a
// data-paired write (SetRollupOffset) fsync inline, while a relaxed commit
// (WithTransactionRelaxed) does not. If a future change accidentally routed a
// data-paired write through the relaxed path (reintroducing the #588
// silent-zeros hazard), the inline-sync count would not advance and this fails.
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

	// SetRollupOffset is data-paired with the append-log fsync → must fsync.
	before = store.InlineSyncCountForTest()
	_, err := store.SetRollupOffset(ctx, "payload-A", 4096)
	require.NoError(t, err)
	require.Greater(t, store.InlineSyncCountForTest(), before,
		"SetRollupOffset must fsync inline (data-paired)")
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
	_, err := store.SetRollupOffset(ctx, "payload-A", 4096)
	require.NoError(t, err)
	require.Equal(t, before, store.InlineSyncCountForTest(),
		"strict mode must fsync via SyncWrites, never inline db.Sync")
}

// TestRelaxedDurability_DurableWriteSurvivesReopen confirms a data-paired write
// on the relaxed store persists across a clean close+reopen with no loss — the
// everyday durability the deferred-fsync path must never compromise.
func TestRelaxedDurability_DurableWriteSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")

	store := openRelaxed(t, dbPath)
	_, err := store.SetRollupOffset(ctx, "payload-A", 8192)
	require.NoError(t, err)
	require.NoError(t, store.Close()) // clean close flushes everything durably

	reopened := openRelaxed(t, dbPath)
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.GetRollupOffset(ctx, "payload-A")
	require.NoError(t, err)
	require.Equal(t, uint64(8192), got, "durable rollup offset must survive close+reopen")
}
