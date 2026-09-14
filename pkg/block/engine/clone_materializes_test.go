package engine_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newCloneEngine builds an engine over a real journal-backed local store whose
// carve path is left unwired, so the source bytes stay in the journal and never
// reach the remote. That is what makes a CLONE-reads-zeros regression visible:
// an in-memory local store would mask it, so this fixture deliberately uses the
// journal store.
func newCloneEngine(t *testing.T, ms metadata.Store) *engine.Store {
	t.Helper()
	localStore, err := journal.Open(t.TempDir(), journal.Config{MaxLocalBytes: 100 * 1024 * 1024,
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.SyncedHashStore", ms)
	}
	coord := &testCoordinator{store: ms}
	testRemote := remotememory.New()
	syncer := engine.NewRemoteSync(localStore, testRemote, ms, engine.DefaultConfig())
	// No remote block store is wired, so the manifest-only sink records the
	// FileChunk rows via the SyncedHashStore committer and nothing uploads.
	bs, err := engine.New(engine.BlockStoreConfig{
		Remote:          testRemote,
		Local:           localStore,
		RemoteSync:      syncer,
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
	n, err := bs.ReadAt(context.Background(), payloadID, out, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt(%s): %v", payloadID, err)
	}
	return out[:n]
}

// TestCloneWholeFile_MaterializesContent guards against a CLONE that copies
// only the source's manifest rows (hash + size) while the bytes they name have
// never left the source's journal: a read of such a clone finds no interval of
// its own and zero-fills, which is silent corruption. The clone must read back
// byte-identical instead.
func TestCloneWholeFile_MaterializesContent(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs := newCloneEngine(t, ms)

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
	if err := ms.UpdateAttrs(ctx, srcFile); err != nil {
		t.Fatalf("UpdateAttrs src size: %v", err)
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
