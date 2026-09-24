package common

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLocalOnlyTestEngine builds a journal-backed engine with no remote, so
// CloneWholeFile takes materializeLocalClone rather than the manifest-only
// reflink. The engine's own tests cover the remote-backed path; this fixture is
// the only way to reach the local-only one.
func newLocalOnlyTestEngine(t *testing.T, coord *fakeCoordinator, ms *metadatamemory.MemoryMetadataStore) (*engine.Store, *journal.Store) {
	t.Helper()
	localStore, err := journal.Open(t.TempDir(), journal.Config{MaxLocalBytes: 100 * 1024 * 1024})
	if err != nil {
		t.Fatalf("journal.Open failed: %v", err)
	}
	syncedHashStore, ok := metadata.Store(ms).(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	syncer := engine.NewRemoteSync(localStore, nil, ms, engine.DefaultConfig())
	syncer.SetSyncedHashStore(syncedHashStore)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		RemoteSync:      syncer,
		FileChunkStore:  ms,
		Coordinator:     coord,
		SyncedHashStore: syncedHashStore,
	})
	if err != nil {
		t.Fatalf("engine.New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, localStore
}

// writeAndSeal writes one payload's whole content and seals it into the
// manifest the way a normal write plus commit does.
func writeAndSeal(t *testing.T, ctx context.Context, bs *engine.Store, payloadID string, data []byte) {
	t.Helper()
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt(%s): %v", payloadID, err)
	}
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush(%s): %v", payloadID, err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}
}

// The local-only clone deliberately shares the whole-source contract. It
// refuses an initially longer destination rather than offering a range copy,
// and leaves both the local content and manifest intact.
func TestMaterializeLocalClone_RejectsLongerDestination(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, _ := newLocalOnlyTestEngine(t, &fakeCoordinator{}, ms)

	const srcSize, dstSize = 4096, 8192
	source := bytes.Repeat([]byte{0x11}, srcSize)
	replaced := bytes.Repeat([]byte{0x22}, dstSize)

	srcHandle := putTestFile(t, ms, "/tail-src.bin", "tail-src-pid", nil, srcSize)
	dstHandle := putTestFile(t, ms, "/tail-dst.bin", "tail-dst-pid", nil, dstSize)
	writeAndSeal(t, ctx, bs, "tail-src-pid", source)
	writeAndSeal(t, ctx, bs, "tail-dst-pid", replaced)

	before, err := ms.GetFile(ctx, dstHandle)
	if err != nil {
		t.Fatal(err)
	}
	beforeRows := mustListChunks(t, ctx, ms, "tail-dst-pid")
	err = CloneWholeFile(ctx, bs, ms, nil, srcHandle, dstHandle, "tail-dst-pid", 0)
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrNotSupported {
		t.Errorf("clone error = %v, want unsupported whole-file replacement", err)
	}
	after, err := ms.GetFile(ctx, dstHandle)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size != dstSize || !reflect.DeepEqual(after.Blocks, before.Blocks) {
		t.Errorf("rejected clone changed destination size/manifest: size=%d want=%d", after.Size, dstSize)
	}
	afterRows := mustListChunks(t, ctx, ms, "tail-dst-pid")
	if !reflect.DeepEqual(afterRows, beforeRows) {
		t.Error("rejected clone changed destination chunk rows")
	}
	back := make([]byte, dstSize)
	if _, err := bs.ReadAt(ctx, "tail-dst-pid", back, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back[:srcSize], replaced[:srcSize]) {
		t.Error("rejected clone overwrote the destination prefix")
	}
	if !bytes.Equal(back[srcSize:], replaced[srcSize:]) {
		t.Error("rejected clone discarded the destination tail")
	}

	// The source is unchanged too; it must still read back intact.
	srcBack := make([]byte, srcSize)
	if _, err := bs.ReadAt(ctx, "tail-src-pid", srcBack, 0); err != nil {
		t.Fatalf("ReadAt(src): %v", err)
	}
	if !bytes.Equal(srcBack, source) {
		t.Error("the clone disturbed the source")
	}
}

// A source longer than the destination remains a supported whole-file clone:
// it grows the destination, and every copied byte must survive.
func TestMaterializeLocalClone_GrowsWithoutClipping(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, _ := newLocalOnlyTestEngine(t, &fakeCoordinator{}, ms)

	const srcSize, dstSize = 8192, 4096
	source := bytes.Repeat([]byte{0x33}, srcSize)

	srcHandle := putTestFile(t, ms, "/grow-src.bin", "grow-src-pid", nil, srcSize)
	dstHandle := putTestFile(t, ms, "/grow-dst.bin", "grow-dst-pid", nil, dstSize)
	writeAndSeal(t, ctx, bs, "grow-src-pid", source)
	writeAndSeal(t, ctx, bs, "grow-dst-pid", bytes.Repeat([]byte{0x44}, dstSize))

	if err := CloneWholeFile(ctx, bs, ms, nil, srcHandle, dstHandle, "grow-dst-pid", 0); err != nil {
		t.Fatalf("CloneWholeFile: %v", err)
	}

	back := make([]byte, srcSize)
	if _, err := bs.ReadAt(ctx, "grow-dst-pid", back, 0); err != nil {
		t.Fatalf("ReadAt(dst): %v", err)
	}
	if !bytes.Equal(back, source) {
		t.Error("the destination does not hold the whole source after a clone that grew it")
	}
}

func mustListChunks(t *testing.T, ctx context.Context, ms *metadatamemory.MemoryMetadataStore, payloadID string) []*block.FileChunk {
	t.Helper()
	rows, err := ms.ListFileChunks(ctx, payloadID)
	if err != nil {
		t.Fatalf("ListFileChunks(%s): %v", payloadID, err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s carved no rows, so the row assertion would hold vacuously", payloadID)
	}
	return rows
}
