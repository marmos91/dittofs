package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// gapInjectingStore commits a manifest write into the window between
// SetFileAttributes' pre-transaction read and its transaction's read. The
// pre-read calls GetFile; the in-transaction read calls it too. The first call
// (the pre-read) is the one that must be treated as untrusted, so the injection
// runs on it: it stands in for a concurrent WRITE that commits after the caller
// has already taken its snapshot.
//
// Only the first call injects, and only once, so the transaction's own read sees
// the concurrent writer's row.
type gapInjectingStore struct {
	*memory.MemoryMetadataStore
	handle    metadata.FileHandle
	injected  []block.ChunkRef
	didInject bool
}

func (s *gapInjectingStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if !s.didInject {
		s.didInject = true
		// Commit the concurrent writer's manifest straight to the store, so
		// the transaction below reads a row the caller's snapshot never saw.
		cur, err := s.MemoryMetadataStore.GetFile(ctx, s.handle)
		if err != nil {
			return nil, err
		}
		next := *cur
		next.Blocks = s.injected
		next.Size = 1 << 20
		// Qualified deliberately: this type overrides GetFile, and the
		// unqualified form reads as if it re-entered that override.
		if err := s.MemoryMetadataStore.SetManifest(ctx, &next); err != nil { //nolint:staticcheck // QF1008: the qualifier names the embedded call explicitly
			return nil, err
		}
	}
	return s.MemoryMetadataStore.GetFile(ctx, handle)
}

// A truncate must prune the block list the committed row holds, not the one the
// caller read before its transaction opened. Pruning the pre-transaction copy
// writes that copy back over whatever a concurrent WRITE committed in the gap,
// discarding its ranges; the row the transaction reads is the only list whose
// tail is actually past the new EOF.
//
// The interleaving is a real race, so this drives it deterministically rather
// than hoping to hit it: the assertion can fail only when the stale list is
// written back, never because the window was missed.
func TestSetFileAttributes_TruncatePrunesTheCommittedManifestNotTheStaleCopy(t *testing.T) {
	const share = "/prune"
	base := memory.NewMemoryMetadataStoreWithDefaults()

	rootFile, err := base.CreateRootDirectory(context.Background(), share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o777,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeShareHandle(share, rootFile.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle: %v", err)
	}

	// The file starts with one block at offset 0, and its pre-read size is
	// small enough that a truncate to 1 byte does NOT prune it: the stale copy
	// would conclude "nothing to prune" and take the attr-only path.
	//
	// Created through the service, before the injecting store is registered, so
	// the create itself does not trip the injection.
	preRead := []block.ChunkRef{{Offset: 0, Size: 4}}
	svc := metadata.New()
	if err := svc.RegisterStoreForShare(share, metadata.Store(base)); err != nil {
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
	file, _, err := svc.CreateFile(ctx, rootHandle, "f", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644, Size: 4, Blocks: preRead,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	// The concurrent writer commits a range the caller never saw, past the
	// truncate point. It must be pruned; the stale list's range at offset 0
	// must be kept.
	concurrent := []block.ChunkRef{
		{Offset: 0, Size: 4},
		{Offset: 4096, Size: 512},
	}
	store := &gapInjectingStore{
		MemoryMetadataStore: base,
		handle:              handle,
		injected:            concurrent,
	}
	if err := svc.RegisterStoreForShare(share, metadata.Store(store)); err != nil {
		t.Fatalf("RegisterStoreForShare (injecting): %v", err)
	}

	// Truncate to 1 byte. The concurrent writer's range at 4096 is past EOF and
	// must go; its range at 0 straddles EOF and is kept.
	if _, err := svc.SetFileAttributes(ctx, handle, &metadata.SetAttrs{
		Size: metadata.Uint64Ptr(1),
	}); err != nil {
		t.Fatalf("SetFileAttributes: %v", err)
	}

	if !store.didInject {
		t.Fatal("precondition: the pre-read never ran, so this test cannot reach the window")
	}

	got, err := svc.GetFile(context.Background(), handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	for _, ref := range got.Blocks {
		if ref.Offset >= 1 {
			t.Errorf("block at offset %d survived a truncate to 1 byte: the stale "+
				"pre-transaction list was written back over the committed row", ref.Offset)
		}
	}
	if len(got.Blocks) == 0 {
		t.Fatal("the block straddling the new EOF was dropped: the prune used the stale list")
	}
}
