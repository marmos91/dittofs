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

	require.NoError(t, ReapSupersededManifest(ctx, tx, pid, [][2]int64{{2500, 5000}}, newOffsets))

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

	require.NoError(t, ReapSupersededManifest(ctx, tx, pid, [][2]int64{{2000, 3000}}, newOffsets))

	require.Equal(t, [][2]int64{
		{0, 1000},
		{1000, 6000}, // kept whole: its tail past 3000 has no other cover
		{2000, 3000},
	}, tiling(t, tx, pid))
}

// TestReapSupersededManifest_KeepsInteriorRowReachingPastRun mirrors the head
// case at the run's tail: a row that starts inside the run but ends past it must
// survive whole. Deleting it — which is what its start offset alone would say to
// do — strands [runEnd, rowEnd) with no cover, and no row can be made to start
// mid-chunk to take that stretch over, so the bytes read back as zeros.
func TestReapSupersededManifest_KeepsInteriorRowReachingPastRun(t *testing.T) {
	const pid = "share/p"
	ctx := context.Background()
	tx := newManifestTx(pid)

	// The run [2000, 3000) stops inside the row at 2500, which reaches 6000.
	seedRows(t, tx, pid, [][2]uint64{{0, 2000}, {2000, 500}, {2500, 3500}})
	// Re-carved: one fresh row tiling the run.
	seedRows(t, tx, pid, [][2]uint64{{2000, 1000}})
	newOffsets := map[int64]struct{}{2000: {}}

	require.NoError(t, ReapSupersededManifest(ctx, tx, pid, [][2]int64{{2000, 3000}}, newOffsets))
	require.Equal(t, [][2]int64{
		{0, 2000},
		{2000, 3000}, // the fresh row; the stale [2000, 2500) it replaced is gone
		{2500, 6000}, // kept whole: its tail past 3000 has no other cover
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

	require.NoError(t, ReapSupersededManifest(ctx, tx, pid, [][2]int64{{2000, 3000}}, newOffsets))

	require.Equal(t, [][2]int64{{0, 2000}, {2000, 3000}}, tiling(t, tx, pid))
}

// TestReapSupersededManifest_ManySpans reaps a whole carve pass in one call: the
// spans are the committed prefixes of the pass's dirty runs, and the un-recarved
// bytes in the holes between them keep every row they had.
func TestReapSupersededManifest_ManySpans(t *testing.T) {
	const pid = "share/p"
	ctx := context.Background()
	tx := newManifestTx(pid)

	// Three 1000-byte rows per 2000-byte span, each span followed by a 1000-byte
	// hole the pass never re-carved.
	seedRows(t, tx, pid, [][2]uint64{
		{0, 1000}, {1000, 1000}, {2000, 1000}, // span [0, 2000) + hole [2000, 3000)
		{3000, 1000}, {4000, 1000}, {5000, 1000}, // span [3000, 5000) + hole
		{6000, 1000}, {7000, 1000}, {8000, 1000}, // span [6000, 8000) + hole
	})
	// Each span re-tiled by one fresh row.
	seedRows(t, tx, pid, [][2]uint64{{0, 2000}, {3000, 2000}, {6000, 2000}})
	newOffsets := map[int64]struct{}{0: {}, 3000: {}, 6000: {}}
	spans := [][2]int64{{0, 2000}, {3000, 5000}, {6000, 8000}}

	require.NoError(t, ReapSupersededManifest(ctx, tx, pid, spans, newOffsets))
	require.Equal(t, [][2]int64{
		{0, 2000},
		{2000, 3000}, // hole: untouched
		{3000, 5000},
		{5000, 6000}, // hole: untouched
		{6000, 8000},
		{8000, 9000}, // hole: untouched
	}, tiling(t, tx, pid))
}

// TestReapSupersededManifest_CostIsIndependentOfSpanCount pins what makes the
// pass-end reap one call rather than one per run: it reads the manifest a fixed
// number of times whatever the run count, so a file with tens of thousands of
// dirty runs does not hold the journal's carve lock for a scan per run.
func TestReapSupersededManifest_CostIsIndependentOfSpanCount(t *testing.T) {
	const pid = "share/p"
	ctx := context.Background()

	reads := func(spanCount int) int {
		tx := newManifestTx(pid)
		var seed [][2]uint64
		var spans [][2]int64
		newOffsets := map[int64]struct{}{}
		for i := 0; i < spanCount; i++ {
			base := uint64(i) * 3000
			seed = append(seed, [2]uint64{base, 1000}, [2]uint64{base + 1000, 1000}, [2]uint64{base + 2000, 1000})
			spans = append(spans, [2]int64{int64(base), int64(base) + 2000})
			newOffsets[int64(base)] = struct{}{}
		}
		seedRows(t, tx, pid, seed)
		for _, sp := range spans {
			seedRows(t, tx, pid, [][2]uint64{{uint64(sp[0]), 2000}})
		}
		tx.lists = 0
		require.NoError(t, ReapSupersededManifest(ctx, tx, pid, spans, newOffsets))
		return tx.lists
	}

	require.Equal(t, reads(1), reads(16), "manifest reads must not scale with the span count")
}

// TestManifestRowEndAfter covers the offset a carve run has to reach so the reap
// never deletes a row whose tail the run leaves uncovered.
func TestManifestRowEndAfter(t *testing.T) {
	const pid = "share/p"
	ctx := context.Background()

	t.Run("no rows", func(t *testing.T) {
		end, err := ManifestRowEndAfter(ctx, newManifestTx(pid), pid, 2000)
		require.NoError(t, err)
		require.Equal(t, int64(2000), end)
	})

	t.Run("row boundary", func(t *testing.T) {
		tx := newManifestTx(pid)
		seedRows(t, tx, pid, [][2]uint64{{0, 2000}, {2000, 1000}})
		end, err := ManifestRowEndAfter(ctx, tx, pid, 2000)
		require.NoError(t, err)
		require.Equal(t, int64(2000), end, "a row starting at the offset does not straddle it")
	})

	t.Run("straddler", func(t *testing.T) {
		tx := newManifestTx(pid)
		seedRows(t, tx, pid, [][2]uint64{{0, 1000}, {1000, 2000}, {3000, 1000}})
		end, err := ManifestRowEndAfter(ctx, tx, pid, 2500)
		require.NoError(t, err)
		require.Equal(t, int64(3000), end)
	})

	t.Run("overlapping chain", func(t *testing.T) {
		// The row at 2800 starts past the probe but inside the straddler, so a
		// single pass would stop at 3000 and still leave 2800..4000 half covered.
		tx := newManifestTx(pid)
		seedRows(t, tx, pid, [][2]uint64{{0, 1000}, {1000, 2000}, {2800, 1200}, {4000, 500}})
		end, err := ManifestRowEndAfter(ctx, tx, pid, 2500)
		require.NoError(t, err)
		require.Equal(t, int64(4000), end)
	})
}
