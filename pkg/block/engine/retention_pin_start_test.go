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

// premiseDrainFreed builds a remote-backed engine, optionally disables eviction
// the way the share constructor does for a pinned share, fills it and reports
// how many local bytes an explicit force-evict reclaims.
func premiseDrainFreed(t *testing.T, pin bool) int64 {
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
	// This is what blockstore_config.go does for a RetentionPin share, before Start.
	if pin {
		localStore.SetEvictionEnabled(false)
	}

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

func TestPremise_ControlUnpinnedEvicts(t *testing.T) {
	if freed := premiseDrainFreed(t, false); freed == 0 {
		t.Fatalf("control: unpinned share freed 0 bytes; the probe cannot detect eviction")
	} else {
		t.Logf("control: unpinned share freed %d bytes", freed)
	}
}

func TestPremise_PinnedShareStillEvictsAfterStart(t *testing.T) {
	freed := premiseDrainFreed(t, true)
	t.Logf("pinned share freed %d bytes", freed)
	if freed != 0 {
		t.Fatalf("PREMISE CONFIRMED: pinned share freed %d local bytes after Start", freed)
	}
}
