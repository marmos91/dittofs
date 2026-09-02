package engine

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// reclaimHook runs before delegating to the wrapped reclaimer, which places it
// exactly in the sweep's decision window: the sweep has already read syncedAt
// and consulted the mark-phase live set, and has not yet freed anything.
type reclaimHook struct {
	inner  BlockReclaimer
	before func(block.ContentHash)
}

func (r reclaimHook) ReclaimDeadChunk(ctx context.Context, h block.ContentHash) (bool, int64, error) {
	r.before(h)
	return r.inner.ReclaimDeadChunk(ctx, h)
}

// TestGCIndexSweep_ConcurrentDedupKeepsBytes pins the invariant that binds the
// carve dedup oracle to the remote sweep: whenever the oracle answers "already
// remote-durable" for a hash, the bytes behind that hash must still be there
// afterwards. The oracle's true answer costs the carver its only copy — the
// carve drops the plaintext and writes a manifest row alone — so a sweep that
// frees the block leaves that file pointing at nothing.
//
// Both orderings of the two decisions are driven deterministically; neither
// subtest depends on goroutine scheduling.
func TestGCIndexSweep_ConcurrentDedupKeepsBytes(t *testing.T) {
	// The oracle wins: a carve deduped onto the hash before the sweep ran. Its
	// manifest row lands after the mark phase, so the live set cannot show it —
	// the sweep must keep the hash anyway.
	t.Run("dedup before sweep", func(t *testing.T) {
		ctx := t.Context()
		rs := remotememory.New()
		defer func() { _ = rs.Close() }()

		rec := newGCMSReconciler()
		st := rec.addShare("share-a")

		h := hashFromString("dedup-race-before")
		seedRemoteChunk(t, st, rs, h) // synced, backdated past grace, no manifest row

		durable, err := (engineDeduper{synced: st}).IsChunkDurable(ctx, journal.ChunkHash(h))
		if err != nil {
			t.Fatalf("IsChunkDurable: %v", err)
		}
		if !durable {
			t.Fatalf("IsChunkDurable = false for a synced hash; fixture no longer exercises a dedup hit")
		}

		stats := collectGarbageBlocks(t, rec, st, rs, &Options{
			GCStateRoot: t.TempDir(),
			GracePeriod: time.Hour,
		})

		if stats.ObjectsSwept != 0 {
			t.Errorf("ObjectsSwept = %d, want 0 (a carve deduped onto the hash)", stats.ObjectsSwept)
		}
		if !chunkOnRemote(t, st, h) {
			t.Fatal("bytes freed under a live dedup adoption: the deduped file's chunk now resolves to nothing")
		}
	})

	// The sweep wins: it reaches the hash before any carve adopts it. The
	// oracle must then refuse, so the carver uploads the chunk instead of
	// pointing at bytes that are being freed as it answers.
	t.Run("dedup during sweep", func(t *testing.T) {
		ctx := t.Context()
		rs := remotememory.New()
		defer func() { _ = rs.Close() }()

		rec := newGCMSReconciler()
		st := rec.addShare("share-a")

		h := hashFromString("dedup-race-during")
		seedRemoteChunk(t, st, rs, h)

		deduper := engineDeduper{synced: st}
		var durable bool
		var dedupErr error
		opts := &Options{
			GCStateRoot: t.TempDir(),
			GracePeriod: time.Hour,
		}
		idx, ok := st.(SyncedHashIndex)
		if !ok {
			t.Fatalf("metadata store %T does not implement SyncedHashIndex", st)
		}
		opts.SyncedHashIndex = idx
		opts.BlockReclaimer = reclaimHook{
			inner: newBlockGCReclaimer(st, rs),
			before: func(swept block.ContentHash) {
				if swept != h {
					return
				}
				durable, dedupErr = deduper.IsChunkDurable(ctx, journal.ChunkHash(swept))
			},
		}

		stats := CollectGarbage(ctx, rec, opts)
		if dedupErr != nil {
			t.Fatalf("IsChunkDurable during sweep: %v", dedupErr)
		}
		if stats.ObjectsSwept != 1 {
			t.Fatalf("ObjectsSwept = %d, want 1 (the hook must fire inside a real reclamation)", stats.ObjectsSwept)
		}
		if durable {
			t.Fatal("dedup oracle claimed a hash the sweep was reclaiming: the carve drops its plaintext and the bytes are freed a moment later")
		}
	})
}
