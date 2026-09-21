package badger

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// putQuotaTestFile writes one regular file row owned by (uid, gid), which moves
// the usage counters by size and one inode.
func putQuotaTestFile(tb testing.TB, store *BadgerMetadataStore, share, path string, uid, gid uint32, size uint64) metadata.FileHandle {
	tb.Helper()
	ctx := context.Background()
	h, err := store.GenerateHandle(ctx, share, path)
	require.NoError(tb, err)
	_, id, err := metadata.DecodeFileHandle(h)
	require.NoError(tb, err)
	f := &metadata.File{
		ShareName: share,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o600,
			UID:       uid,
			GID:       gid,
			PayloadID: metadata.PayloadID(path),
			Size:      size,
		},
	}
	f.ID = id
	require.NoError(tb, store.UpdateAttrs(ctx, f))
	return h
}

// openQuotaStore opens (or reopens) a store at a fixed path so a test can close
// it and come back to the same keyspace.
func openQuotaStore(tb testing.TB, dir string) *BadgerMetadataStore {
	tb.Helper()
	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), filepath.Join(dir, "metadata.db"))
	require.NoError(tb, err)
	return store
}

// TestQuotaCountersSeedOpenWithoutScanningFileRows is the test this whole change
// exists for. Correct usage numbers after a reopen prove nothing on their own —
// a store that silently kept scanning every file row would report exactly the
// same numbers. The assertion that matters is that the reopen decoded no file
// rows at all.
func TestQuotaCountersSeedOpenWithoutScanningFileRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store := openQuotaStore(t, dir)
	createShareRoot(t, store, "/q")
	for i := 0; i < 8; i++ {
		putQuotaTestFile(t, store, "/q", fmt.Sprintf("/f%d", i), 1000, 100, 1024)
	}
	before, err := store.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	// Guards against a vacuous test: if writing rows charged nothing, every
	// assertion below would hold over zeros.
	require.Equal(t, int64(8*1024), before)
	require.NoError(t, store.Close())

	reopened := openQuotaStore(t, dir)
	defer func() { _ = reopened.Close() }()

	require.Zero(t, reopened.UsageScanCount(),
		"a reopen of a backfilled store must read the counters, not re-derive them from the file rows")

	after, err := reopened.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, before, after)

	usage, err := reopened.GetQuotaUsage("/q", metadata.QuotaScopeUser, 1000)
	require.NoError(t, err)
	require.Equal(t, int64(8*1024), usage.Bytes)
	require.Equal(t, int64(8), usage.Files)
}

// TestQuotaCounterStripesHoldSignedPartials pins the decision recorded on
// decodeUsageStat: a stripe is not clamped at zero on its own.
//
// The stripe a create lands on is not the stripe its delete lands on, so after
// a create/delete pair one stripe holds a positive partial and another the
// matching negative. Clamping either in isolation would leave the bucket
// permanently over-counted — charging a user for a file that no longer exists,
// which is the direction that wrongly denies writes.
func TestQuotaCounterStripesHoldSignedPartials(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store := openQuotaStore(t, dir)
	createShareRoot(t, store, "/q")
	gone := putQuotaTestFile(t, store, "/q", "/gone", 1000, 100, 4096)

	used, err := store.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, int64(4096), used)

	// A separate transaction, so its delta round-robins onto a different stripe
	// than the create's.
	require.NoError(t, store.DeleteFile(ctx, gone))

	require.NoError(t, store.Close())

	reopened := openQuotaStore(t, dir)
	defer func() { _ = reopened.Close() }()

	used, err = reopened.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Zero(t, used, "the delete's negative partial must survive to be summed against the create's")

	usage, err := reopened.GetQuotaUsage("/q", metadata.QuotaScopeUser, 1000)
	require.NoError(t, err)
	require.Zero(t, usage.Bytes)
	require.Zero(t, usage.Files)
}

// TestRecomputeUsageRepairsDivergedCounters covers the realign an operator runs.
// Live-maintained counters have no self-correction, so the repair path is the
// only thing standing between a drift bug and permanently wrong quota.
func TestRecomputeUsageRepairsDivergedCounters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store := openQuotaStore(t, dir)
	createShareRoot(t, store, "/q")
	for i := 0; i < 4; i++ {
		putQuotaTestFile(t, store, "/q", fmt.Sprintf("/f%d", i), 1000, 100, 2048)
	}
	want, err := store.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, int64(4*2048), want)

	// Corrupt a stripe behind the store's back, the way a drift bug would.
	require.NoError(t, store.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(
			keyQuotaUsage("/q", metadata.QuotaScopeUser, 1000, 3),
			encodeUsageStat(metadata.UsageStat{Bytes: 999999, Files: 42}),
		)
	}))
	require.NoError(t, store.Close())

	diverged := openQuotaStore(t, dir)
	got, err := diverged.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.NotEqual(t, want, got, "the corruption must actually be visible, or the repair below proves nothing")

	require.NoError(t, diverged.RecomputeUsage(ctx))
	repaired, err := diverged.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, want, repaired)
	require.NoError(t, diverged.Close())

	// The repair has to have reached the durable counters, not just the cache.
	final := openQuotaStore(t, dir)
	defer func() { _ = final.Close() }()
	require.Zero(t, final.UsageScanCount())
	persisted, err := final.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, want, persisted)
}

// TestQuotaCountersBackfillLegacyStore covers the one scan a store pays: an
// existing keyspace whose rows predate the counters.
func TestQuotaCountersBackfillLegacyStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store := openQuotaStore(t, dir)
	createShareRoot(t, store, "/q")
	putQuotaTestFile(t, store, "/q", "/a", 1000, 100, 512)
	want, err := store.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)

	// Strip the counters and the marker, leaving the file rows: exactly the
	// shape of a store opened for the first time after the upgrade.
	require.NoError(t, store.db.DropPrefix([]byte(prefixQuotaUsage)))
	require.NoError(t, store.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Delete(keyQuotaCountersBackfilled)
	}))
	require.NoError(t, store.Close())

	upgraded := openQuotaStore(t, dir)
	require.Equal(t, uint64(1), upgraded.UsageScanCount(), "the first open after upgrade derives the counters once")
	got, err := upgraded.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NoError(t, upgraded.Close())

	// And the scan is a debt paid once, not per open.
	again := openQuotaStore(t, dir)
	defer func() { _ = again.Close() }()
	require.Zero(t, again.UsageScanCount())
	got, err = again.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestInterruptedRealignSelfHeals covers the window where the durable counters
// have been dropped but their replacement has not landed.
//
// The rewrite is two operations — DropPrefix, then a WriteBatch — and badger
// cannot make them one. If the store still claimed its counters accounted for
// every row, the next open would take them at face value and report every
// identity as having dropped to zero, with nothing left consulting the file rows
// to notice. So the marker is withdrawn before the drop, and the interrupted
// state is one the next open repairs.
func TestInterruptedRealignSelfHeals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store := openQuotaStore(t, dir)
	createShareRoot(t, store, "/q")
	for i := 0; i < 3; i++ {
		putQuotaTestFile(t, store, "/q", fmt.Sprintf("/f%d", i), 1000, 100, 8192)
	}
	want, err := store.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, int64(3*8192), want)

	// Reproduce the state a rewrite killed between its two steps leaves behind,
	// through the same helper the real path uses.
	require.NoError(t, clearQuotaCountersBackfilled(store.db))
	require.NoError(t, store.db.DropPrefix([]byte(prefixQuotaUsage)))
	require.NoError(t, store.Close())

	healed := openQuotaStore(t, dir)
	defer func() { _ = healed.Close() }()

	require.Equal(t, uint64(1), healed.UsageScanCount(),
		"an open finding no trustworthy counters must re-derive them from the file rows")
	got, err := healed.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, want, got, "usage must come back, not reset to zero")
}
