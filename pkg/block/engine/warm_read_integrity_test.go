package engine_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newEngineWithRemote mirrors newEngineOverStore but wires a real (memory)
// remote + block store + synced-hash store, so the #1636 readahead driver
// actually fires (scheduleReadahead no-ops without a healthy remote). This is
// the faithful warm-read shape: data lives locally AND is uploaded to the
// remote, no eviction.
func newEngineWithRemote(t *testing.T, ms metadata.Store, mem *remotememory.Store) *engine.Store {
	t.Helper()
	if _, ok := ms.(metadata.RollupStore); !ok {
		t.Fatalf("metadata store %T does not implement metadata.RollupStore", ms)
	}
	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 3_600_000, // async rollup never fires; explicit DrainRollups only
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	coord := &testCoordinator{store: ms}
	syncer := engine.NewSyncer(localStore, mem, ms, engine.DefaultConfig())
	syncer.SetSyncedHashStore(syncedHashStore)
	syncer.SetRemoteBlockStore(mem)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Syncer:          syncer,
		FileChunkStore:  ms,
		Coordinator:     coord,
		SyncedHashStore: syncedHashStore,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// TestWarmReadIntegrity_AfterDrainUploads reproduces the blocks-only warm-read
// data-integrity bug: write ~16 MiB, roll up + upload to the remote (NO
// eviction), then read the whole file back in 1 MiB windows via the local warm
// read path (Store.ReadAt) and compare each window's blake3 to the source.
//
// The #1636 readahead driver fires on every ReadAt with a healthy remote wired,
// so demand reads run concurrently with prefetch workers. Run under -race.
func TestWarmReadIntegrity_AfterDrainUploads(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		runWarmReadIntegrity(t, metadatamemory.NewMemoryMetadataStoreWithDefaults())
	})
	t.Run("badger", func(t *testing.T) {
		ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
		}
		defer func() { _ = ms.Close() }()
		runWarmReadIntegrity(t, ms)
	})
}

func runWarmReadIntegrity(t *testing.T, ms metadata.Store) {
	ctx := context.Background()
	mem := remotememory.New()
	bs := newEngineWithRemote(t, ms, mem)

	rootHandle := createShare(t, ms, "warmread")
	pid, _ := createRealFile(t, ms, "warmread", "big.bin", rootHandle)

	const oneMiB = 1024 * 1024
	const fileSize = 16 * oneMiB
	src := make([]byte, fileSize)
	rng := rand.New(rand.NewSource(0x5EED)) //nolint:gosec // deterministic test fixture
	rng.Read(src)

	// Write in 1 MiB windows to mimic SMB's sequential 1 MiB WRITE RPCs.
	for off := 0; off < fileSize; off += oneMiB {
		if _, err := bs.WriteAt(ctx, pid, nil, src[off:off+oneMiB], uint64(off)); err != nil {
			t.Fatalf("WriteAt off=%d: %v", off, err)
		}
	}
	if _, err := bs.Flush(ctx, pid); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Roll up (append log -> CAS + manifest) then upload to the remote. NO evict.
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}

	// Per-window expected blake3.
	nWin := fileSize / oneMiB
	wantHash := make([][32]byte, nWin)
	for i := 0; i < nWin; i++ {
		wantHash[i] = blake3.Sum256(src[i*oneMiB : (i+1)*oneMiB])
	}

	// Read the whole file back in 1 MiB sliding windows, several passes,
	// concurrently, to drive the readahead sliding window hard.
	const passes = 20
	const readers = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(format string, args ...interface{}) {
		mu.Lock()
		if firstErr == nil {
			firstErr = fmt.Errorf(format, args...)
		}
		mu.Unlock()
	}

	// verify reads [off,off+len) and checks it byte-matches src, poisoning the
	// buffer first so any offset the local assembly leaves uncovered (the
	// recycled-non-zeroed-buffer amplifier) surfaces as a mismatch rather than
	// hiding behind zeros.
	verify := func(buf []byte, off uint64) {
		for i := range buf {
			buf[i] = 0xAA // poison
		}
		n, err := bs.ReadAt(ctx, pid, nil, buf, off)
		if err != nil {
			fail("ReadAt off=%d len=%d: %v", off, len(buf), err)
			return
		}
		if n != len(buf) {
			fail("ReadAt off=%d short read n=%d want=%d", off, n, len(buf))
			return
		}
		want := src[off : off+uint64(len(buf))]
		if blake3.Sum256(buf) != blake3.Sum256(want) {
			// Find first mismatch for a precise message.
			for j := range buf {
				if buf[j] != want[j] {
					fail("WARM READ MISMATCH off=%d len=%d: first bad byte at +%d got=0x%02x want=0x%02x (poison=0xAA)",
						off, len(buf), j, buf[j], want[j])
					return
				}
			}
			fail("WARM READ MISMATCH off=%d len=%d (hash differs, no byte diff?)", off, len(buf))
		}
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			buf1 := make([]byte, oneMiB)
			// Odd, non-MiB-aligned window like a real SMB max-read (drives the
			// sliding readahead window across chunk boundaries at odd offsets).
			const odd = 512*1024 + 7
			bufOdd := make([]byte, odd)
			whole := make([]byte, fileSize)
			for p := 0; p < passes; p++ {
				// Pass A: sequential 1 MiB windows.
				for i := 0; i < nWin; i++ {
					verify(buf1, uint64(i)*oneMiB)
				}
				// Pass B: odd-sized sequential windows.
				for off := uint64(0); off+odd <= fileSize; off += odd {
					verify(bufOdd, off)
				}
				// Pass C: one big whole-file read.
				verify(whole, 0)
			}
			_ = seed
		}(r)
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatal(firstErr)
	}
}
