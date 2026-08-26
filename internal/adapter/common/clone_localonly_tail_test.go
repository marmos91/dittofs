package common

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLocalOnlyTestEngine builds a journal-backed engine with no remote, so
// CloneWholeFile takes materializeLocalClone rather than the manifest-only
// reflink. The engine's own tests cover the remote-backed path; this fixture is
// the only way to reach the local-only one.
func newLocalOnlyTestEngine(t *testing.T, coord *fakeCoordinator, ms *metadatamemory.MemoryMetadataStore) (*engine.Store, *fs.FSStore) {
	t.Helper()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions failed: %v", err)
	}
	syncedHashStore, ok := metadata.Store(ms).(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	syncer := engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())
	syncer.SetSyncedHashStore(syncedHashStore)
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Syncer:          syncer,
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

// TestMaterializeLocalClone_KeepsNothingPastTheSourcesSize pins the half of the
// clone contract the local-only path used to leave undone.
//
// That path copies real bytes over [0, srcSize) and lets the write path
// supersede the destination's own intervals by version. Superseding is not
// clipping, so a destination that was longer than the source keeps everything
// past srcSize — interval, manifest row and all. The size stamped on the
// destination hides it from every read that clamps, which is why it stays
// invisible until something grows the file: a grown region owes zeros, and the
// destination would serve the bytes the clone was supposed to take away.
//
// The assertions are on both tiers deliberately. Clipping only the local tier
// would leave a row claiming bytes the file no longer has, and the two tiers
// disagreeing about who owns a range is its own defect — one that reads as a
// mosaic rather than as a clean stale read.
func TestMaterializeLocalClone_KeepsNothingPastTheSourcesSize(t *testing.T) {
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

	if err := CloneWholeFile(ctx, bs, ms, nil, srcHandle, dstHandle, "tail-dst-pid"); err != nil {
		t.Fatalf("CloneWholeFile: %v", err)
	}

	// The tail is the defect: past the source's size the destination must hold
	// nothing, so the block store zero-fills it the way it does any range a file
	// never had.
	tail := make([]byte, dstSize-srcSize)
	if _, err := bs.ReadAt(ctx, "tail-dst-pid", tail, srcSize); err != nil {
		t.Fatalf("ReadAt(dst) past the source's size: %v", err)
	}
	if bytes.Equal(tail, replaced[srcSize:]) {
		t.Error("past the source's size the destination still serves the content the clone replaced")
	}
	if !bytes.Equal(tail, make([]byte, len(tail))) {
		t.Errorf("past the source's size the destination served %x…, want zeros", tail[:8])
	}

	// The control: the clip must take the tail and nothing else. Without this a
	// clip that emptied the destination outright would pass the assertion above.
	head := make([]byte, srcSize)
	if _, err := bs.ReadAt(ctx, "tail-dst-pid", head, 0); err != nil {
		t.Fatalf("ReadAt(dst) over the copied range: %v", err)
	}
	if !bytes.Equal(head, source) {
		t.Error("the destination does not hold the content the clone gave it")
	}

	// The other tier: no row may go on claiming bytes the file no longer has.
	for _, r := range mustListChunks(t, ctx, ms, "tail-dst-pid") {
		off, ok := block.ParseChunkOffset(r.ID)
		if !ok {
			continue
		}
		if end := off + uint64(r.DataSize); end > srcSize {
			t.Errorf("row %s claims [%d, %d), past the destination's new size %d", r.ID, off, end, srcSize)
		}
	}

	// The source is a bystander: the clip is the destination's, and reading the
	// source back proves the fixture is not simply broken for every payload.
	srcBack := make([]byte, srcSize)
	if _, err := bs.ReadAt(ctx, "tail-src-pid", srcBack, 0); err != nil {
		t.Fatalf("ReadAt(src): %v", err)
	}
	if !bytes.Equal(srcBack, source) {
		t.Error("the clone disturbed the source")
	}
}

// TestMaterializeLocalClone_GrowsWithoutClipping is the other direction, and
// what keeps the clip from ever being the thing that loses content: a source
// longer than the destination grows it, and every copied byte must survive.
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

	if err := CloneWholeFile(ctx, bs, ms, nil, srcHandle, dstHandle, "grow-dst-pid"); err != nil {
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
