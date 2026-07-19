package engine_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLocalOnlyEngine builds an engine over a real journal-backed local store
// with NO remote store (HasRemoteStore() == false). This is the configuration
// that surfaces the local-only CLONE-reads-zeros bug: the journal owns the only
// copy of the bytes, so a manifest-only reflink leaves the destination with no
// resolvable interval. An in-memory local store would mask the bug, so this
// fixture deliberately uses the fs (journal) store.
func newLocalOnlyEngine(t *testing.T, ms metadata.Store) *engine.Store {
	t.Helper()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	coord := &testCoordinator{store: ms}
	syncer := engine.NewSyncer(localStore, nil, ms, engine.DefaultConfig())
	// No remote block store: the journal owns the bytes and the local carve sink
	// records the FileChunk manifest via the SyncedHashStore committer.
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

// readWhole reads exactly len(dst) bytes of payloadID from offset 0.
func readWhole(t *testing.T, bs *engine.Store, payloadID string, size int) []byte {
	t.Helper()
	out := make([]byte, size)
	n, err := bs.ReadAt(context.Background(), payloadID, nil, out, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt(%s): %v", payloadID, err)
	}
	return out[:n]
}

// TestCloneWholeFile_LocalOnly_MaterializesContent guards the residual #1784
// bug: on a share with no remote store, CLONE used to copy only the source's
// manifest rows (hash + size) to the destination, which carries no journal
// interval of its own — so a read of the clone found no interval and zero-filled
// (silent corruption). The fix materializes the source bytes into the
// destination's own journal, so the clone reads back byte-identical.
func TestCloneWholeFile_LocalOnly_MaterializesContent(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs := newLocalOnlyEngine(t, ms)

	root := createShare(t, ms, "clone")
	srcPID, srcHandle := createRealFile(t, ms, "clone", "src.bin", root)
	dstPID, dstHandle := createRealFile(t, ms, "clone", "dst.bin", root)

	// 3 MiB of deterministic but non-trivial content so a byte copy spans
	// multiple carve chunks and any accidental zero-fill is obvious.
	const size = 3 << 20
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i*131 + 7)
	}
	if _, err := bs.WriteAt(ctx, srcPID, nil, src, 0); err != nil {
		t.Fatalf("WriteAt src: %v", err)
	}
	if _, err := bs.Flush(ctx, srcPID); err != nil {
		t.Fatalf("Flush src: %v", err)
	}
	// Persist the source size the way a real WRITE path would; CloneWholeFile
	// reads it to bound the materialize copy.
	srcFile, err := ms.GetFile(ctx, srcHandle)
	if err != nil {
		t.Fatalf("GetFile src: %v", err)
	}
	srcFile.Size = size
	if err := ms.PutFile(ctx, srcFile); err != nil {
		t.Fatalf("PutFile src size: %v", err)
	}

	// CLONE over a local-only share.
	if err := common.CloneWholeFile(ctx, bs, ms, nil, srcHandle, dstHandle, metadata.PayloadID(dstPID)); err != nil {
		t.Fatalf("CloneWholeFile: %v", err)
	}

	// The destination reads back byte-identical (would be all zeros before the
	// fix).
	got := readWhole(t, bs, dstPID, size)
	if len(got) != size {
		t.Fatalf("clone read size = %d, want %d", len(got), size)
	}
	if !bytes.Equal(got, src) {
		if bytes.Equal(got, make([]byte, size)) {
			t.Fatalf("clone read back all zeros — local-only CLONE did not materialize bytes")
		}
		t.Fatalf("clone content differs from source")
	}

	// The destination's File.Size was stamped from the source.
	dstFile, err := ms.GetFile(ctx, dstHandle)
	if err != nil {
		t.Fatalf("GetFile dst: %v", err)
	}
	if dstFile.Size != size {
		t.Fatalf("dst File.Size = %d, want %d", dstFile.Size, size)
	}

	// Copy-on-write: overwriting the destination head must not disturb the
	// source (each file owns an independent payload/journal).
	patch := bytes.Repeat([]byte{0x55}, 64<<10)
	if _, err := bs.WriteAt(ctx, dstPID, nil, patch, 0); err != nil {
		t.Fatalf("WriteAt dst patch: %v", err)
	}
	if _, err := bs.Flush(ctx, dstPID); err != nil {
		t.Fatalf("Flush dst: %v", err)
	}
	srcAfter := readWhole(t, bs, srcPID, size)
	if !bytes.Equal(srcAfter, src) {
		t.Fatalf("source mutated after writing the clone — COW violated")
	}
	dstAfter := readWhole(t, bs, dstPID, size)
	if !bytes.Equal(dstAfter[:len(patch)], patch) {
		t.Fatalf("clone head did not reflect the new write")
	}
	if !bytes.Equal(dstAfter[len(patch):], src[len(patch):]) {
		t.Fatalf("clone tail lost the cloned content after the head write")
	}
}
