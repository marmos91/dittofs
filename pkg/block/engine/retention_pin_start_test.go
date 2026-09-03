package engine_test

import (
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// drainAfterFill builds a remote-backed engine the way the share constructor
// does — pinning the local store before Start when the share's retention policy
// is pin — fills it with a file that is rolled up and uploaded, then force-evicts
// and reports how many local bytes were reclaimed.
func drainAfterFill(t *testing.T, pinned bool) int64 {
	t.Helper()
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()

	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	localStore.SetEvictionPinned(pinned)

	syncer := engine.NewSyncer(localStore, mem, ms, engine.DefaultConfig())
	syncer.SetSyncedHashStore(ms)
	syncer.SetRemoteBlockStore(mem)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Syncer:          syncer,
		FileChunkStore:  ms,
		Coordinator:     &testCoordinator{store: ms},
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	// Start probes the remote, finds it healthy and reconciles eviction against
	// that health. The pin must survive it.
	if err := bs.Start(ctx); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	rootHandle := createShare(t, ms, "pinshare")
	pid, _ := createRealFile(t, ms, "pinshare", "big.bin", rootHandle)

	const oneMiB = 1024 * 1024
	const fileSize = 16 * oneMiB
	src := make([]byte, fileSize)
	rand.New(rand.NewSource(0x2257)).Read(src) //nolint:gosec // deterministic fixture
	for off := 0; off < fileSize; off += oneMiB {
		if _, err := bs.WriteAt(ctx, pid, nil, src[off:off+oneMiB], uint64(off)); err != nil {
			t.Fatalf("WriteAt off=%d: %v", off, err)
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
	freed, err := bs.DrainLocalSynced(ctx)
	if err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	return freed
}

// TestRetentionPinSurvivesStart covers the pin being lifted by the eviction
// reconcile at the end of engine Start: the constructor pinned the local store,
// Start found the remote healthy, and every synced local byte then became
// evictable again.
//
// The unpinned subtest is the control — without it a pinned share reporting
// "freed 0" proves only that the probe cannot evict anything at all.
func TestRetentionPinSurvivesStart(t *testing.T) {
	t.Run("unpinned evicts", func(t *testing.T) {
		if freed := drainAfterFill(t, false); freed == 0 {
			t.Fatal("unpinned share freed 0 bytes; the probe cannot observe eviction")
		}
	})
	t.Run("pinned keeps its bytes", func(t *testing.T) {
		if freed := drainAfterFill(t, true); freed != 0 {
			t.Fatalf("pinned share freed %d local bytes; the retention pin was lifted", freed)
		}
	})
}
