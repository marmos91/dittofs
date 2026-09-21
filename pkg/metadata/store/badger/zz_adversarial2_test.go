package badger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// regular -> non-regular in-place type change: does the charge get refunded?
func TestADV_TypeChangeLeaksCounter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := openQuotaStore(t, dir)
	createShareRoot(t, store, "/q")

	h := putQuotaTestFile(t, store, "/q", "/f", 1000, 100, 8192)
	used, err := store.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	require.Equal(t, int64(8192), used)

	// Same inode, now a symlink. It holds no logical bytes any more.
	_, id, err := metadata.DecodeFileHandle(h)
	require.NoError(t, err)
	f := &metadata.File{
		ShareName: "/q",
		Path:      "/f",
		FileAttr: metadata.FileAttr{
			Type:       metadata.FileTypeSymlink,
			Mode:       0o777,
			UID:        1000,
			GID:        100,
			LinkTarget: "/elsewhere",
			Size:       10,
		},
	}
	f.ID = id
	require.NoError(t, store.UpdateAttrs(ctx, f))

	used, err = store.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	t.Logf("in-memory used after regular->symlink: %d (truth: 0)", used)

	require.NoError(t, store.Close())
	re := openQuotaStore(t, dir)
	defer func() { _ = re.Close() }()
	persisted, err := re.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	t.Logf("after reopen (durable counters): %d, scans=%d", persisted, re.UsageScanCount())

	require.NoError(t, re.RecomputeUsage(ctx))
	truth, err := re.GetUsedBytesForShare(ctx, "/q")
	require.NoError(t, err)
	t.Logf("after RecomputeUsage (truth from rows): %d", truth)

	require.Equal(t, truth, persisted, "durable counters disagree with the file rows")
}
