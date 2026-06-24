package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newPipelineHarness wires a full engine over an fs local store (rollup ticker
// running) + an in-memory remote (periodic uploader running), with the given
// stabilization window. Returns the engine, the remote, the metadata store.
func newPipelineHarness(t *testing.T, stabilizationMS int) (*engine.Store, *memory.Store, *metadatamemory.MemoryMetadataStore) {
	t.Helper()
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rem := memory.New()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: stabilizationMS,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	if err := localStore.StartRollup(ctx); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	syncer := engine.NewSyncer(localStore, rem, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          rem,
		Syncer:          syncer,
		FileBlockStore:  ms,
		Coordinator:     &testCoordinator{store: ms},
		SyncedHashStore: ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, rem, ms
}

// TestRepro1403_SustainedWritesStall characterizes the actual #1403 mechanism:
// while a file is written CONTINUOUSLY (the streaming-NFS case), the 250ms
// stabilization window keeps the head interval "fresh", so the rollup ticker
// never finds a stable interval and nothing rolls up — the append log just
// grows. Once writes STOP, the interval stabilizes and the pipeline drains
// normally. This is a design gap (no max-stabilization-age bound), not a
// broken pipeline or a regression.
func TestRepro1403_SustainedWritesStall(t *testing.T) {
	ctx := context.Background()
	bs, rem, ms := newPipelineHarness(t, 250) // production default stabilization

	rootHandle := createShare(t, ms, "s1")
	payloadID, handle := createRealFile(t, ms, "s1", "stream.bin", rootHandle)

	// Phase 1: write continuously for ~1.2s (no pause longer than stabilization).
	chunk := make([]byte, 64*1024)
	for i := range chunk {
		chunk[i] = byte(i*13 + 1)
	}
	var blocks []block.BlockRef
	var offset uint64
	stop := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(stop) {
		cur, err := bs.WriteAt(ctx, payloadID, blocks, chunk, offset)
		if err != nil {
			t.Fatalf("WriteAt @%d: %v", offset, err)
		}
		blocks = cur
		offset += uint64(len(chunk))
		// vary content so each write is a fresh dirty interval
		chunk[0]++
	}
	duringWrites := len(fileBlocks(t, ms, handle))
	t.Logf("during continuous writes: %d CAS blocks rolled up", duringWrites)

	// Phase 2: close the file (Flush). The contributor's "upload a file" flow
	// ends in a close → Flush, which is documented to IGNORE UploadDelay and
	// mirror immediately. If data is still missing from the remote after this,
	// it is a real bug — not just the 10s periodic-upload delay.
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush (file close): %v", err)
	}
	var drained bool
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		fb := fileBlocks(t, ms, handle)
		if len(fb) > 0 {
			all := true
			for _, b := range fb {
				if has, _ := rem.Has(ctx, b.Hash); !has {
					all = false
					break
				}
			}
			if all {
				t.Logf("after writes stopped: drained %d blocks to remote in %.1fs", len(fb), time.Since(stop).Seconds())
				drained = true
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !drained {
		t.Fatalf("data never reached remote even after writes stopped (worse than the stabilization gap)")
	}
	// The characterization: continuous writes did NOT roll up promptly; the
	// drain only happened after writes ceased.
	if duringWrites > 0 {
		t.Logf("note: %d blocks rolled up mid-stream (stabilization gap smaller than expected on this machine)", duringWrites)
	}
}

// TestRepro1403_IdleWriteReachesRemote drives the FULL engine pipeline through
// the append log (NOT a direct StoreChunk): write 4 MiB to a file, let the
// rollup ticker + periodic uploader run, and check whether the data (a) became
// CAS chunks locally and (b) reached the remote. It deliberately uses a SHORT
// stabilization window and a real rollup pool + periodic uploader so the IDLE
// path is exercised — the case the contributor reported broken (#1403).
func TestRepro1403_IdleWriteReachesRemote(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	// MemoryMetadataStore implements both RollupStore and SyncedHashStore.
	rollupStore := ms
	syncedHashStore := ms

	rem := memory.New()

	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 100, // short: an idle interval stabilizes in 100ms
		RollupStore:     rollupStore,
		SyncedHashStore: syncedHashStore,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	// Launch the rollup ticker pool — the production server does this at
	// service.go:CreateLocalStoreFromConfig.
	if err := localStore.StartRollup(ctx); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}

	syncer := engine.NewSyncer(localStore, rem, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          rem,
		Syncer:          syncer,
		FileBlockStore:  ms,
		Coordinator:     &testCoordinator{store: ms},
		SyncedHashStore: syncedHashStore,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	// Launch the periodic uploader (the live S3 mirror path).
	if err := bs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer bs.Close()

	rootHandle := createShare(t, ms, "s1")
	payloadID, handle := createRealFile(t, ms, "s1", "f.bin", rootHandle)

	data := make([]byte, 4*1024*1024)
	for i := range data {
		data[i] = byte(i * 7) // non-zero, dedup-resistant content
	}
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Poll up to 8s for the idle pipeline to: roll up → produce CAS chunks →
	// mirror to the remote. No snapshot, no forced drain.
	var lastBlocks []block.BlockRef
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		lastBlocks = fileBlocks(t, ms, handle)
		if len(lastBlocks) > 0 {
			allOnRemote := true
			for _, b := range lastBlocks {
				has, _ := rem.Has(ctx, b.Hash)
				if !has {
					allOnRemote = false
					break
				}
			}
			if allOnRemote {
				t.Logf("SUCCESS: %d CAS blocks created and all mirrored to remote", len(lastBlocks))
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Diagnose exactly which link is dead.
	casCreated := len(lastBlocks)
	onRemote := 0
	for _, b := range lastBlocks {
		if has, _ := rem.Has(ctx, b.Hash); has {
			onRemote++
		}
	}
	switch {
	case casCreated == 0:
		t.Fatalf("DEAD LINK = ROLLUP: after 8s idle, 0 CAS blocks created (data stranded in append log; rollup ticker never produced chunks)")
	case onRemote == 0:
		t.Fatalf("DEAD LINK = MIRROR: %d CAS blocks created but 0 reached the remote (rollup OK; onChunkComplete→addPendingHash→mirror broken)", casCreated)
	default:
		t.Fatalf("PARTIAL: %d CAS blocks, only %d on remote", casCreated, onRemote)
	}
}
