package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

func truncateObjectIDTruncate(t *testing.T, svc *metadata.Service, ctx *metadata.AuthContext,
	h metadata.FileHandle, size uint64) {
	t.Helper()

	if _, err := svc.SetFileAttributes(ctx, h, &metadata.SetAttrs{
		Size: metadata.Uint64Ptr(size),
	}); err != nil {
		t.Fatalf("truncate to %d: %v", size, err)
	}
}

func truncateObjectIDSetup(t *testing.T) (*metadata.Service, *metadata.AuthContext, metadata.FileHandle) {
	t.Helper()
	const share = "/truncate-objectid"
	store := badgertest.NewInMemory(t)
	rootFile, err := store.CreateRootDirectory(context.Background(), share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o777,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}

	root, err := metadata.EncodeShareHandle(share, rootFile.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle: %v", err)
	}

	svc := metadata.New()
	if err := svc.RegisterStoreForShare(share, metadata.Store(store)); err != nil {
		t.Fatalf("RegisterStoreForShare: %v", err)
	}

	ctx := &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0), GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}

	return svc, ctx, root
}

func truncateObjectIDCreate(t *testing.T, svc *metadata.Service, ctx *metadata.AuthContext,
	root metadata.FileHandle, name string, blocks []block.ChunkRef, oid block.ObjectID) metadata.FileHandle {
	t.Helper()
	f, _, err := svc.CreateFile(ctx, root, name, &metadata.FileAttr{
		Type:     metadata.FileTypeRegular,
		Mode:     0o644,
		Size:     8192,
		Blocks:   append([]block.ChunkRef(nil), blocks...),
		ObjectID: oid,
	})

	if err != nil {
		t.Fatalf("CreateFile(%s): %v", name, err)
	}

	h, err := metadata.EncodeFileHandle(f)
	if err != nil {
		t.Fatalf("EncodeFileHandle(%s): %v", name, err)
	}

	// Precondition: the file starts with exactly the chunks and ObjectID asked for.
	got, err := svc.GetFile(context.Background(), h)
	if err != nil {
		t.Fatalf("GetFile(%s): %v", name, err)
	}

	if len(got.Blocks) != len(blocks) || got.ObjectID != oid {
		t.Fatalf("precondition %s: Blocks=%v ObjectID=%x, want %d blocks and ObjectID %x",
			name, got.Blocks, got.ObjectID, len(blocks), oid)
	}

	return h
}

func truncateObjectIDAssert(t *testing.T, svc *metadata.Service, h metadata.FileHandle,
	size uint64, wantOID block.ObjectID) {
	t.Helper()
	got, err := svc.GetFile(context.Background(), h)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}

	if got.Size != size {
		t.Errorf("Size = %d, want %d", got.Size, size)
	}

	if len(got.Blocks) != 1 || got.Blocks[0].Offset != 0 {
		t.Errorf("Blocks = %v, want only the ref at offset 0", got.Blocks)
	}

	if got.ObjectID != wantOID {
		t.Errorf("ObjectID = %x, want %x", got.ObjectID, wantOID)
	}
}

// A size-down truncate that drops a chunk ref must not give the file an
// ObjectID it did not have. ComputeObjectID hashes only the chunk hashes, and
// the store keeps ObjectIDs unique, so two files left with the same chunks
// would collide and the second truncate would fail on every retry (#2924).
func TestSetFileAttributes_TruncateObjectID(t *testing.T) {
	h1 := block.ChunkRef{Hash: block.ContentHash{0x01}, Offset: 0, Size: 4096}
	h2 := block.ChunkRef{Hash: block.ContentHash{0x02}, Offset: 4096, Size: 4096}
	h3 := block.ChunkRef{Hash: block.ContentHash{0x03}, Offset: 4096, Size: 4096}

	t.Run("same chunks, same size", func(t *testing.T) {
		svc, ctx, root := truncateObjectIDSetup(t)
		a := truncateObjectIDCreate(t, svc, ctx, root, "a.bin", []block.ChunkRef{h1, h2}, block.ObjectID{})
		b := truncateObjectIDCreate(t, svc, ctx, root, "b.bin", []block.ChunkRef{h1, h2}, block.ObjectID{})

		truncateObjectIDTruncate(t, svc, ctx, a, 100)
		truncateObjectIDTruncate(t, svc, ctx, b, 100)

		truncateObjectIDAssert(t, svc, a, 100, block.ObjectID{})
		truncateObjectIDAssert(t, svc, b, 100, block.ObjectID{})
	})

	t.Run("shared first chunk, different sizes", func(t *testing.T) {
		svc, ctx, root := truncateObjectIDSetup(t)
		a := truncateObjectIDCreate(t, svc, ctx, root, "a.bin", []block.ChunkRef{h1, h2}, block.ObjectID{})
		b := truncateObjectIDCreate(t, svc, ctx, root, "b.bin", []block.ChunkRef{h1, h3}, block.ObjectID{})

		truncateObjectIDTruncate(t, svc, ctx, a, 100)
		truncateObjectIDTruncate(t, svc, ctx, b, 200)

		truncateObjectIDAssert(t, svc, a, 100, block.ObjectID{})
		truncateObjectIDAssert(t, svc, b, 200, block.ObjectID{})
	})

	t.Run("existing ObjectID follows the remaining chunks", func(t *testing.T) {
		svc, ctx, root := truncateObjectIDSetup(t)
		full := []block.ChunkRef{h1, h2}
		f := truncateObjectIDCreate(t, svc, ctx, root, "legacy.bin", full, block.ComputeObjectID(full))

		truncateObjectIDTruncate(t, svc, ctx, f, 100)

		truncateObjectIDAssert(t, svc, f, 100, block.ComputeObjectID([]block.ChunkRef{h1}))
	})
}
