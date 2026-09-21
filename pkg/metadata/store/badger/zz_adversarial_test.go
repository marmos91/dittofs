package badger

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Simulates: restore a PRE-UPGRADE dump into a freshly-created badger store
// (the conformance/CLI shape, no Reset first).
func TestADV_RestoreLegacyDumpIntoFreshStore(t *testing.T) {
	ctx := context.Background()

	srcDir := t.TempDir()
	src := openQuotaStore(t, srcDir)
	createShareRoot(t, src, "/q")
	for i := 0; i < 4; i++ {
		putQuotaTestFile(t, src, "/q", "/f"+string(rune('a'+i)), 1000, 100, 1024)
	}
	want, err := src.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, int64(4*1024), want)

	// Make the dump look like one taken before this change: no qu: counters,
	// no marker. The payload-index marker is dropped too, as a pre-#pl store.
	require.NoError(t, src.db.DropPrefix([]byte(prefixQuotaUsage)))
	require.NoError(t, src.db.Update(func(txn *badgerdb.Txn) error {
		if err := txn.Delete(keyQuotaCountersBackfilled); err != nil {
			return err
		}
		return txn.Delete(keyPayloadIndexBackfilled)
	}))

	var dump bytes.Buffer
	_, err = src.WriteSnapshot(ctx, &dump)
	require.NoError(t, err)
	require.NoError(t, src.Close())

	// A brand-new destination store, opened normally (this is what the CLI /
	// the conformance suite does) and then restored into.
	dstDir := t.TempDir()
	dst := openQuotaStore(t, dstDir)
	t.Logf("fresh dst UsageScanCount=%d", dst.UsageScanCount())

	haveCounters, err := quotaCountersBackfilled(dst.db)
	require.NoError(t, err)
	needIndex, err := payloadIndexBackfillNeeded(dst.db)
	require.NoError(t, err)
	t.Logf("fresh dst: haveCounters=%v needIndex=%v", haveCounters, needIndex)

	require.NoError(t, dst.RestoreSnapshot(ctx, &dump))
	defer func() { _ = dst.Close() }()

	got, err := dst.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	usage, err := dst.GetQuotaUsage("/q", metadata.QuotaScopeUser, 1000)
	require.NoError(t, err)
	t.Logf("after restore: used=%d want=%d  usage=%+v  scans=%d", got, want, usage, dst.UsageScanCount())
	require.Equal(t, want, got, "restored share reports the wrong usage")
	_ = filepath.Join
}
