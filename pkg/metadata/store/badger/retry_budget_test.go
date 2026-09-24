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
// Every writer appends to the chunk manifest of one file, so every attempt reads
// the f:<id> / fm:<id> pair and writes it back and badger's SSI aborts all but
// one of them. The conflicts are the workload, not the defect. What must never
// happen is one reaching the caller: a serialization event the loop exists to
// absorb turns into an I/O error at the protocol layer.
//
// The guard reads the store's conflict counter rather than timing the
// serialization, so it asserts nothing about wall-clock and does not care how
// loaded the machine is. Two assertions carry it: no commit returns an error,
// and the conflict count is non-zero — without the latter the workload might
// have committed first-try throughout and proved nothing about the retry path.
//
// Writer count is what supplies the contention, deliberately rather than
// per-commit cost. Eight writers — the reported case — only exhaust a fixed
// attempt budget when each commit is slow enough to widen the conflict window,
// which takes an already-large manifest to re-encode; the workload's total cost
// then becomes what the test measures, and on a slow or loaded machine it, not
// the backoff, decides the outcome. A wider herd over a cheap key produces the
// same re-collisions with commits fast enough that the whole run drains in a
// fraction of the retry budget on any machine.
//
// It also pins the other half of the contract: each attempt appends onto the
// list it read inside its own transaction, so a retried attempt re-derives from
// whatever committed in between and no committed append is lost.
func TestWithTransaction_HotFileNeverSurfacesConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("contention probe; skipped under -short")
	}

	const (
		writers   = 32
		perWriter = 8
	)

	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	root := mkPayloadShare(t, store, "/s")
	handle := mkPayloadFile(t, store, "/s", root, "hot.bin", "/hot.bin", "/s/hot")

	var failures, committed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := range perWriter {
				off := uint64(w*perWriter+i) << 20
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

	// Checked before the error assertion below so a run that does surface a
	// conflict still reports whether a committed append was lost with it.
	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	offsets := make(map[uint64]struct{}, len(got.Blocks))
	for _, b := range got.Blocks {
		offsets[b.Offset] = struct{}{}
	}
	want := int(committed.Load())
	require.Len(t, got.Blocks, want,
		"a retried append re-proposed a stale list and dropped a committed chunk")
	require.Len(t, offsets, want, "an offset was appended twice")

	require.Zero(t, failures.Load(),
		"a hot file's SSI conflicts must stay inside the retry loop, not reach the caller")
	require.Positive(t, conflicts,
		"the workload never contended, so it says nothing about the retry path")
}
