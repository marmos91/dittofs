package metadata

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
)

// tiling renders the manifest as (offset, end) pairs in offset order so a test
// can state the whole post-reap shape in one assertion.
func tiling(t *testing.T, tx *manifestTx, payloadID string) [][2]int64 {
	t.Helper()
	rows, err := tx.ListFileChunks(context.Background(), payloadID)
	require.NoError(t, err)
	out := make([][2]int64, 0, len(rows))
	for _, r := range rows {
		off, ok := block.ParseChunkOffset(r.ID)
		require.True(t, ok)
		out = append(out, [2]int64{int64(off), int64(off) + int64(r.DataSize)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// seedRows writes rows tiling the given (offset, size) pairs.
func seedRows(t *testing.T, tx *manifestTx, payloadID string, spans [][2]uint64) {
	t.Helper()
	for i, sp := range spans {
		require.NoError(t, tx.Put(context.Background(), chunkRow(payloadID, sp[0], i, uint32(sp[1]))))
	}
}

// TestReapSupersededManifest_NarrowsStraddler pins the tiling invariant across a
// carve run that starts in the middle of an existing row. The row starting before
// the run cannot be deleted — it is the only cover for the bytes before the run —
// and it cannot be left whole either, or it overlaps the run's fresh rows. It is
// narrowed to the prefix the run did not re-chunk.
func TestReapSupersededManifest_NarrowsStraddler(t *testing.T) {
	const pid = "share/p"
	ctx := context.Background()
	tx := newManifestTx(pid)

	// Pre-run manifest tiles [0, 6000): the row at 1000 straddles the run start,
	// the row at 3000 is interior to the run, the row at 5000 is a cold remainder
	// past the run.
	seedRows(t, tx, pid, [][2]uint64{{0, 1000}, {1000, 2000}, {3000, 2000}, {5000, 1000}})

	// The run re-carved [2500, 5000) into two fresh rows.
	seedRows(t, tx, pid, [][2]uint64{{2500, 1200}, {3700, 1300}})
	newOffsets := map[int64]struct{}{2500: {}, 3700: {}}

	require.NoError(t, ReapSupersededManifest(ctx, tx, pid, 2500, 5000, newOffsets))

	require.Equal(t, [][2]int64{
		{0, 1000},
		{1000, 2500}, // narrowed from end 3000
		{2500, 3700},
		{3700, 5000},
		{5000, 6000}, // cold remainder, untouched
	}, tiling(t, tx, pid))
}

// TestReapSupersededManifest_KeepsStraddlerReachingPastRun covers the one
// straddler that must survive whole: it also ends past the run, and no row can
// start mid-chunk, so narrowing it would trade the overlap for a gap over
// [runEnd, rowEnd) — bytes that would then read back as zeros.
func TestReapSupersededManifest_KeepsStraddlerReachingPastRun(t *testing.T) {
	const pid = "share/p"
	ctx := context.Background()
	tx := newManifestTx(pid)

	seedRows(t, tx, pid, [][2]uint64{{0, 1000}, {1000, 5000}})
	seedRows(t, tx, pid, [][2]uint64{{2000, 1000}})
	newOffsets := map[int64]struct{}{2000: {}}

	require.NoError(t, ReapSupersededManifest(ctx, tx, pid, 2000, 3000, newOffsets))

	require.Equal(t, [][2]int64{
		{0, 1000},
		{1000, 6000}, // kept whole: its tail past 3000 has no other cover
		{2000, 3000},
	}, tiling(t, tx, pid))
}

// TestReapSupersededManifest_LeavesDisjointRowsAlone keeps the reap from
// touching a row that merely ends where the run begins.
func TestReapSupersededManifest_LeavesDisjointRowsAlone(t *testing.T) {
	const pid = "share/p"
	ctx := context.Background()
	tx := newManifestTx(pid)

	seedRows(t, tx, pid, [][2]uint64{{0, 2000}, {2000, 1000}})
	newOffsets := map[int64]struct{}{2000: {}}

	require.NoError(t, ReapSupersededManifest(ctx, tx, pid, 2000, 3000, newOffsets))

	require.Equal(t, [][2]int64{{0, 2000}, {2000, 3000}}, tiling(t, tx, pid))
}
