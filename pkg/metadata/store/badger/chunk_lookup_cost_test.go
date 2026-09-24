package badger

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// costCeilingPerChunk bounds allocations per sparse lookup, per chunk in the
// payload.
//
// The lookup answers correctly whether it scans the index once per rejected
// candidate or once in total, so no correctness assertion can tell the two
// apart — which is how the per-candidate scan went unrecorded as a linear cost.
// Only a count of work observes it. Allocations are the count to use here:
// unlike wall clock they do not move with machine speed or load, so the
// threshold means the same thing on a laptop and on a loaded runner.
//
// Measured at 5,000 chunks: ~28 allocations per chunk when the scan runs once,
// ~5,045 when it runs per candidate. The ceiling sits an order of magnitude
// above the first and two below the second, so it survives ordinary drift in
// what a scan allocates and still fails outright if the scan returns to
// per-candidate.
const costCeilingPerChunk = 200

// TestGetFileChunkAtOffsetSparseCostStaysLinear pins the cost class of a hole
// lookup, which RFC 4 §12.5 requires and no other test in this package covers.
// The benchmark next door reports the same numbers but asserts nothing, so it
// cannot fail.
func TestGetFileChunkAtOffsetSparseCostStaysLinear(t *testing.T) {
	const chunks = 2000

	ctx := t.Context()
	s := newSizeTestStore(t)
	require.NoError(t, s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		for i := range chunks {
			if err := tx.Put(ctx, &metadata.FileChunk{
				ID:       fmt.Sprintf("cost-gate/%d", i*4096),
				DataSize: 4096,
			}); err != nil {
				return err
			}
		}
		return nil
	}))

	// A hole past the last chunk: every candidate is rejected, so this is the
	// query that pays per-candidate rescanning if it is reintroduced.
	off := uint64(chunks*4096 + 1)
	row, err := s.GetFileChunkAtOffset(ctx, "cost-gate", off)
	require.NoError(t, err)
	require.Nil(t, row, "offset past the last chunk must read as a hole")

	allocs := testing.AllocsPerRun(3, func() {
		if _, err := s.GetFileChunkAtOffset(ctx, "cost-gate", off); err != nil {
			t.Fatal(err)
		}
	})

	perChunk := allocs / float64(chunks)
	t.Logf("%d chunks: %.0f allocations per sparse lookup, %.1f per chunk",
		chunks, allocs, perChunk)
	require.Less(t, perChunk, float64(costCeilingPerChunk),
		"a sparse lookup allocates %.1f times per chunk, over the %d ceiling: the index "+
			"scan is running more than once per lookup", perChunk, costCeilingPerChunk)
}
