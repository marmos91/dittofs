package engine_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// gatedRemote stalls the first chunk read until release is closed, so a fetch
// that resolved its manifest rows before a mutation writes its bytes back after
// that mutation has been carved.
type gatedRemote struct {
	*remotememory.Store
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (g *gatedRemote) ReadChunk(ctx context.Context, blockID string, offset, length int64, hash block.ContentHash) ([]byte, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Store.ReadChunk(ctx, blockID, offset, length, hash)
}

func newEngineWithGatedRemote(t *testing.T, ms metadata.Store, rem *gatedRemote) *engine.Store {
	t.Helper()
	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	syncer := engine.NewSyncer(localStore, rem, ms, engine.DefaultConfig())
	syncer.SetSyncedHashStore(syncedHashStore)
	syncer.SetRemoteBlockStore(rem)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Syncer:          syncer,
		FileChunkStore:  ms,
		Coordinator:     &testCoordinator{store: ms},
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

// TestHydrateDoesNotResurrectOverMutation pins that a remote fetch in flight
// across a mutation cannot put the pre-mutation bytes back. The fetch resolves
// its covering rows, stalls in the remote read, and only writes back after the
// punch has been carved and made durable locally. The punched range must still
// read as zeros.
func TestHydrateDoesNotResurrectOverMutation(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rem := &gatedRemote{Store: remotememory.New(), release: make(chan struct{}), entered: make(chan struct{})}
	bs := newEngineWithGatedRemote(t, ms, rem)

	rootHandle := createShare(t, ms, "hydraterace")
	pid, _ := createRealFile(t, ms, "hydraterace", "f.bin", rootHandle)

	const fileSize = 6 * 1024 * 1024
	const punchLen = 4 * 1024 * 1024
	orig := bytes.Repeat([]byte{0xAB}, fileSize)
	if _, err := bs.WriteAt(ctx, pid, nil, orig, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}

	// A cold read that resolves the pre-punch manifest and then stalls.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		one := make([]byte, 1)
		_, _ = bs.ReadAt(ctx, pid, one, 0)
	}()
	select {
	case <-rem.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("remote read never started")
	}

	// The mutation lands and is made durable while that fetch is stalled.
	if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), 0, punchLen); err != nil {
		t.Fatalf("PunchHole: %v", err)
	}
	carve(t, bs, ctx, pid)

	close(rem.release)
	<-readDone

	got := make([]byte, punchLen)
	if _, err := bs.ReadAt(ctx, pid, got, 0); err != nil {
		t.Fatalf("ReadAt after punch: %v", err)
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("punched range resurrected: byte %d = %#x (want 0); %d nonzero of %d",
				i, b, punchLen-bytes.Count(got, []byte{0}), punchLen)
		}
	}
}
