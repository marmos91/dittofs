package sqlite_test

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// TestScopedManifestCommitDoesNotScaleWithChunkCount asserts that folding a
// single committed chunk into a file's manifest reads a bounded number of
// stored file_block_refs rows regardless of how many chunks the file already
// has. Before the commit path carried its changed-offset set, the diff read
// every stored row, so this cost grew with the file.
func TestScopedManifestCommitDoesNotScaleWithChunkCount(t *testing.T) {
	small := scopedCommitRowsScanned(t, 64)
	large := scopedCommitRowsScanned(t, 4096)

	if small != large || large > 4 {
		t.Fatalf("scoped commit read %d stored rows on a 64-chunk file and %d on a 4096-chunk file; want an equal count bounded by the changed offsets", small, large)
	}
}

// scopedCommitRowsScanned seeds a file with chunks chunk refs, then commits one
// replacement chunk through the projection path and reports how many stored
// manifest rows that commit's diff read.
func scopedCommitRowsScanned(t *testing.T, chunks int) int64 {
	t.Helper()
	ctx := t.Context()

	store, err := sqlite.NewSQLiteMetadataStore(ctx, &sqlite.SQLiteMetadataStoreConfig{
		Path:        t.TempDir() + "/m.db",
		AutoMigrate: true,
	}, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("NewSQLiteMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const share = "/scope"
	if _, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}

	handle, err := store.GenerateHandle(ctx, share, "/f")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}

	const payloadID = "payload-scope"
	const chunkSize = 4096
	refs := make([]block.ChunkRef, chunks)
	for i := range refs {
		refs[i] = block.ChunkRef{
			Hash:   hashOf(1, byte(i), byte(i>>8)),
			Offset: uint64(i) * chunkSize,
			Size:   chunkSize,
		}
	}

	// Seed the manifest in one unscoped write — the state a large file reaches
	// before the commit under test.
	if err := store.PutFile(ctx, &metadata.File{
		ShareName: share,
		Path:      "/f",
		ID:        id,
		FileAttr: metadata.FileAttr{
			Type:        metadata.FileTypeRegular,
			Mode:        0o644,
			Size:        uint64(chunks) * chunkSize,
			PayloadID:   metadata.PayloadID(payloadID),
			Blocks:      refs,
			BlocksDirty: true,
		},
	}); err != nil {
		t.Fatalf("PutFile (seed %d chunks): %v", chunks, err)
	}

	// Commit a single replacement chunk in the middle of the file, the way a
	// carve commit does.
	target := refs[chunks/2]
	target.Hash = hashOf(2, 0xff, 0xff)
	baseline := store.PutFileChunkRefsManifestRowsScanned()
	if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return metadata.ProjectCommittedChunks(ctx, tx, payloadID, []*block.FileChunk{{
			ID:       fmt.Sprintf("%s/%d", payloadID, target.Offset),
			Hash:     target.Hash,
			DataSize: target.Size,
		}})
	}); err != nil {
		t.Fatalf("ProjectCommittedChunks: %v", err)
	}
	scanned := store.PutFileChunkRefsManifestRowsScanned() - baseline

	// The replacement must be visible, and every other offset untouched.
	got, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if len(got.Blocks) != chunks {
		t.Fatalf("manifest length changed: got %d blocks, want %d", len(got.Blocks), chunks)
	}
	for i, ref := range got.Blocks {
		want := refs[i]
		if i == chunks/2 {
			want.Hash = target.Hash
		}
		if ref != want {
			t.Fatalf("block %d: got %+v, want %+v", i, ref, want)
		}
	}
	return scanned
}

func hashOf(b ...byte) block.ContentHash {
	var h block.ContentHash
	copy(h[:], b)
	return h
}
