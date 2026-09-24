package badger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// These tests guard the manifest segmentation against the growth it was written
// to stop (see manifestSegmentRefs for the mechanism).
//
// The first two are regression guards and fail on the unsegmented
// implementation, each on its own headline assertion. Neither asserts
// correctness — the unsegmented store returned exactly the right chunk list
// while leaking — so what they measure is size and bytes written. The third
// guards behaviour the unsegmented store had no way to get wrong, because it
// had only ever one value to delete; it is here so a later change cannot start
// leaving segments behind.

// dirBytes sums the store directory by file extension, which is what separates
// live LSM data (.sst) from value-log accumulation (.vlog).
func dirBytes(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	require.NoError(t, filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		if ext == "" {
			ext = "(none)"
		}
		out[ext] += info.Size()
		return nil
	}))
	return out
}

// manifestSize reports the manifest's total encoded bytes, its largest single
// segment, and how many segments it occupies. The largest segment is the number
// the defect turns on: a value at or above ValueThreshold leaves the LSM for the
// value log.
func manifestSize(t *testing.T, s *BadgerMetadataStore, id uuid.UUID) (total, largest int64, segments int) {
	t.Helper()
	require.NoError(t, s.db.View(func(txn *badgerdb.Txn) error {
		prefix := keyFileManifestPrefix(id)
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			sz := it.Item().ValueSize()
			total += sz
			largest = max(largest, sz)
			segments++
		}
		return nil
	}))
	return total, largest, segments
}

// TestManifestSegmentStaysUnderValueThreshold is the size guard. However many
// chunks a file carries, no single manifest value may reach ValueThreshold —
// that is what keeps the manifest in the LSM, where compaction reclaims
// superseded copies, instead of in the value log, where they accumulate.
func TestManifestSegmentStaysUnderValueThreshold(t *testing.T) {
	ctx := context.Background()

	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	threshold := store.BadgerOptions().ValueThreshold
	root := mkPayloadShare(t, store, "/s")

	t.Logf("ValueThreshold = %d bytes", threshold)
	t.Logf("%8s %10s %12s %9s %s", "chunks", "segments", "total bytes", "largest", "storage")

	for _, n := range []int{100, 4000, 12000, 40000} {
		path := fmt.Sprintf("/f%d.bin", n)
		h := mkChunkedFile(t, store, "/s", root, filepath.Base(path), path, n)
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(t, err)

		total, largest, segments := manifestSize(t, store, id)
		where := "LSM (inline)"
		if largest >= threshold {
			where = "VALUE LOG"
		}
		t.Logf("%8d %10d %12d %9d %s", n, segments, total, largest, where)

		require.Less(t, largest, threshold,
			"a %d-chunk manifest put %d bytes in one value; at or above the %d-byte threshold it goes to the value log, which is the leak",
			n, largest, threshold)

		// The list must still round-trip whole across its segments.
		got, err := store.GetFile(ctx, h)
		require.NoError(t, err)
		require.Len(t, got.Blocks, n, "segmented manifest must reassemble every ref")
	}
}

// TestManifestAppendDoesNotRewriteWholeList is the amplification guard, and the
// axis RFC 4 §12.2 asks for: a commit's cost must be bounded by what it changed,
// not by the file. Appending one chunk at EOF touches the last segment only, so
// the store must grow by roughly one segment per append rather than by the whole
// manifest.
//
// A correctness assertion cannot stand in for this. The unsegmented store
// returned exactly the right chunk list while writing the entire manifest on
// every commit; only counting bytes observes it.
func TestManifestAppendDoesNotRewriteWholeList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A small value-log file size keeps any regression visible within the
	// test's budget rather than hidden inside one preallocated file.
	opts := badgerdb.DefaultOptions(dir).
		WithValueLogFileSize(16 << 20).
		WithLoggingLevel(badgerdb.WARNING)

	store, err := NewBadgerMetadataStore(ctx, BadgerMetadataStoreConfig{
		DBPath:        dir,
		BadgerOptions: &opts,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	threshold := store.BadgerOptions().ValueThreshold
	root := mkPayloadShare(t, store, "/s")

	// Enough chunks that the unsegmented manifest would have cleared the
	// threshold — that is the regime the defect lives in.
	const startChunks = 10000
	const appends = 300

	h := mkChunkedFile(t, store, "/s", root, "big.bin", "/big.bin", startChunks)
	_, id, err := metadata.DecodeFileHandle(h)
	require.NoError(t, err)

	// Logged, deliberately not asserted: on the unsegmented implementation this
	// would fail here and the value-log assertion below — the one this test
	// exists for — would never be reached. TestManifestSegmentStaysUnderValueThreshold
	// is what guards the threshold.
	total, largest, segments := manifestSize(t, store, id)
	t.Logf("manifest: %d bytes across %d segments, largest %d (threshold %d)",
		total, segments, largest, threshold)

	before := dirBytes(t, dir)
	t.Logf("before appends: %v", before)

	file, err := store.GetFile(ctx, h)
	require.NoError(t, err)

	for i := range appends {
		var hash block.ContentHash
		hash[0], hash[1], hash[2] = 0xAB, byte(i), byte(i>>8)
		file.Blocks = append(file.Blocks, block.ChunkRef{
			Hash:   hash,
			Offset: uint64(startChunks+i) << 20,
			Size:   1 << 20,
		})
		file.Size = uint64(startChunks+i+1) << 20
		require.NoError(t, store.SetManifest(ctx, file))
	}

	after := dirBytes(t, dir)
	t.Logf("after %d appends: %v", appends, after)

	vlogGrowth := after["vlog"] - before["vlog"]
	t.Logf("value log grew %d bytes over %d appends", vlogGrowth, appends)

	// The manifest never reaches the value log, so appending to it must not
	// push anything there. Unsegmented, this grew by ~332 MiB.
	require.Zero(t, vlogGrowth,
		"appending to a manifest must not write to the value log; it grew %d bytes", vlogGrowth)

	// Every ref must still be there and in order.
	reloaded, err := store.GetFile(ctx, h)
	require.NoError(t, err)
	require.Len(t, reloaded.Blocks, startChunks+appends)
	for i := 1; i < len(reloaded.Blocks); i++ {
		require.Greater(t, reloaded.Blocks[i].Offset, reloaded.Blocks[i-1].Offset,
			"segments must reassemble in offset order")
	}
}

// TestManifestShrinkDropsSurplusSegments guards the other direction: a truncate
// that shortens the list must remove the segments it no longer reaches, or a
// later read reassembles refs past EOF from segments nothing rewrote.
//
// Unlike the two above this is not a regression guard — an unsegmented store
// had one value and deleting it was the whole job, so it passes this too once
// the multi-segment fixture is taken away. It pins the cleanup that only exists
// now, and it scans the segment prefix rather than walking sequences, so a
// segment orphaned past the end of the list fails it.
func TestManifestShrinkDropsSurplusSegments(t *testing.T) {
	ctx := context.Background()

	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	root := mkPayloadShare(t, store, "/s")
	h := mkChunkedFile(t, store, "/s", root, "big.bin", "/big.bin", 12000)
	_, id, err := metadata.DecodeFileHandle(h)
	require.NoError(t, err)

	_, _, segments := manifestSize(t, store, id)
	require.Greater(t, segments, 1, "the fixture must span several segments to test this")

	// Truncate to one segment's worth of chunks.
	file, err := store.GetFile(ctx, h)
	require.NoError(t, err)
	file.Blocks = block.PruneChunkRefsToSize(file.Blocks, 100<<20)
	file.Size = 100 << 20
	require.NoError(t, store.SetManifest(ctx, file))

	_, _, segments = manifestSize(t, store, id)
	require.Equal(t, 1, segments, "surplus segments must be dropped, not left behind")

	reloaded, err := store.GetFile(ctx, h)
	require.NoError(t, err)
	require.Len(t, reloaded.Blocks, 100, "no ref past the new size may survive")

	// Truncate to zero must leave no segment at all.
	file, err = store.GetFile(ctx, h)
	require.NoError(t, err)
	file.Blocks = block.PruneChunkRefsToSize(file.Blocks, 0)
	file.Size = 0
	require.NoError(t, store.SetManifest(ctx, file))

	_, _, segments = manifestSize(t, store, id)
	require.Zero(t, segments, "truncate to zero must remove every segment")
}
