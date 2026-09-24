package badger

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestWithTransaction_HotFileNeverSurfacesConflict is the retry-budget guard.
//
// Eight writers append to the chunk manifest of one already-large file. Every
// attempt reads the f:<id> / fm:<id> pair and writes it back, so all eight
// overlap on the same keys and badger's SSI aborts the losers — the conflicts
// are the workload, not the defect. What must never happen is a conflict
// reaching the caller: a serialization event the loop exists to absorb turns
// into an I/O error at the protocol layer.
//
// The guard reads the store's conflict counter rather than timing the
// serialization, so it asserts nothing about wall-clock and does not care how
// loaded the machine is. Two assertions carry it: no commit returns an error,
// and the conflict count is non-zero — without the latter the workload might
// have committed first-try throughout and proved nothing about the retry path.
//
// The seeded manifest is what makes the contention real. Each commit re-encodes
// and rewrites a manifest segment, which widens the transaction window enough
// for the eight writers to overlap several times per commit; against an empty
// file the commits are too short to collide more than occasionally.
func TestWithTransaction_HotFileNeverSurfacesConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("contention probe; skipped under -short")
	}

	const (
		writers   = 8
		perWriter = 25
		seed      = 8000
	)

	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	root := mkPayloadShare(t, store, "/s")
	handle := mkChunkedFile(t, store, "/s", root, "hot.bin", "/hot.bin", seed)

	var failures, committed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := range perWriter {
				off := uint64(seed+w*perWriter+i) << 20
				err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
					f, getErr := tx.GetFile(ctx, handle)
					if getErr != nil {
						return getErr
					}
					var h block.ContentHash
					h[0], h[1] = byte(w), byte(i)
					f.Blocks = append(f.Blocks, block.ChunkRef{
						Hash: h, Offset: off, Size: 1 << 20,
					})
					f.Size = max(f.Size, off+(1<<20))
					return tx.SetManifest(ctx, f)
				})
				if err != nil {
					failures.Add(1)
					t.Logf("writer %d append %d: %v", w, i, err)
					continue
				}
				committed.Add(1)
			}
		}(w)
	}
	close(start)
	wg.Wait()

	conflicts := store.TransactionConflictsForTest()
	t.Logf("%d writers x %d appends: %d commits, %d SSI conflicts, %d errors",
		writers, perWriter, writers*perWriter, conflicts, failures.Load())

	// Every append that committed must still be in the manifest. Each attempt
	// appended onto the list it read inside its own transaction, so a retried
	// attempt re-derives from whatever committed in the meantime rather than
	// re-proposing the list its first attempt started from. Checked before the
	// error assertion below so a run that does surface a conflict still reports
	// whether any committed append was lost.
	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	offsets := make(map[uint64]struct{}, len(got.Blocks))
	for _, b := range got.Blocks {
		offsets[b.Offset] = struct{}{}
	}
	want := seed + int(committed.Load())
	require.Len(t, got.Blocks, want,
		"a retried append re-proposed a stale list and dropped a committed chunk")
	require.Len(t, offsets, want, "an offset was appended twice")

	require.Zero(t, failures.Load(),
		"a hot file's SSI conflicts must stay inside the retry loop, not reach the caller")
	require.Positive(t, conflicts,
		"the workload never contended, so it says nothing about the retry path")
}
