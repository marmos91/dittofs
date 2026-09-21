package shares

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newEvictShare registers a share backed by a real journal and a memory remote,
// writes payloadMiB of data and uploads all of it. The caller decides what, if
// anything, is left dirty afterwards.
func newEvictShare(t *testing.T, name string, payloadMiB int) (*Service, *engine.Store) {
	t.Helper()
	ctx := context.Background()

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	local, err := journal.Open(t.TempDir(), journal.Config{})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	mem := remotememory.New()
	cfg := engine.DefaultConfig()
	cfg.ManualSync = true // the test drives every carve; no dispatcher racing it
	sync := engine.NewRemoteSync(local, mem, mds, cfg)
	sync.SetSyncedHashStore(mds)
	sync.SetRemoteBlockStore(mem)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           local,
		Remote:          mem,
		RemoteSync:      sync,
		FileChunkStore:  mds,
		SyncedHashStore: mds,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(ctx); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	const pid = "evict/payload.bin"
	oneMiB := make([]byte, 1<<20)
	for i := range oneMiB {
		oneMiB[i] = byte(i)
	}
	for i := range payloadMiB {
		oneMiB[0] = byte(i)
		if _, err := bs.WriteAt(ctx, pid, nil, oneMiB, uint64(i)<<20); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	}
	if _, err := bs.Flush(ctx, pid); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}

	svc := New()
	svc.InjectShareForTesting(&Share{Name: name, Enabled: true, BlockStore: bs})
	return svc, bs
}

// TestEvictBlockStore_ReportsWhatItReclaimedAndWhyNot is the regression guard
// for an operator-invoked evict reporting success having freed nothing.
//
// Eviction reclaims whole segments and keeps any segment holding a record that
// is not yet on the remote, so a few un-uploaded bytes hold their entire
// segment — and every synced byte sharing it — resident. That is correct: those
// bytes are the only copy. What was missing is any way for the caller to tell
// it apart from "there was nothing left to free", because the result counted
// files (the length of the share's file list, reported whether or not anything
// was evicted) and carried no reason.
//
// The assertions are on the result the admin API returns, not on the journal's
// own EvictResult: the CLI, the REST handler and the bench harness all read
// this struct, and a reason that stops short of it helps nobody.
func TestEvictBlockStore_ReportsWhatItReclaimedAndWhyNot(t *testing.T) {
	ctx := context.Background()

	// Control: everything synced. Without it, "freed 0" in the pinned case
	// would prove only that the probe cannot evict anything at all.
	t.Run("fully synced frees its segments", func(t *testing.T) {
		svc, _ := newEvictShare(t, "/clean", 8)

		res, err := svc.EvictBlockStore(ctx, "/clean", EvictOptions{LocalOnly: true})
		if err != nil {
			t.Fatalf("EvictBlockStore: %v", err)
		}
		if res.SegmentsEvicted == 0 {
			t.Errorf("SegmentsEvicted = 0 with every byte synced; the probe cannot observe eviction")
		}
		if res.BytesFreed == 0 {
			t.Errorf("BytesFreed = 0 with every byte synced")
		}
		if res.UnsyncedBytesPinned != 0 {
			t.Errorf("UnsyncedBytesPinned = %d with every byte synced", res.UnsyncedBytesPinned)
		}
		if res.EvictionHeld {
			t.Errorf("EvictionHeld on a healthy store")
		}
	})

	t.Run("a dirty straggler pins the rest and says so", func(t *testing.T) {
		svc, bs := newEvictShare(t, "/pinned", 8)

		// One small write that never gets carved: the residue an incomplete
		// drain leaves behind. It is a rounding error next to the 8 MiB that is
		// already remote-durable, and it holds all of it locally.
		if _, err := bs.WriteAt(ctx, "evict/payload.bin", nil, []byte("straggler"), 64<<20); err != nil {
			t.Fatalf("straggler WriteAt: %v", err)
		}

		res, err := svc.EvictBlockStore(ctx, "/pinned", EvictOptions{LocalOnly: true})
		if err != nil {
			t.Fatalf("EvictBlockStore: %v", err)
		}
		if res.BytesFreed != 0 || res.SegmentsEvicted != 0 {
			t.Fatalf("evicted %d segments / %d bytes while a record was still un-uploaded; "+
				"eviction must never drop the only copy of dirty bytes",
				res.SegmentsEvicted, res.BytesFreed)
		}
		if res.UnsyncedBytesPinned <= 0 {
			t.Errorf("UnsyncedBytesPinned = %d after an evict that freed nothing; "+
				"the caller is told it succeeded with no way to learn the data was ineligible rather than absent",
				res.UnsyncedBytesPinned)
		}
	})
}
