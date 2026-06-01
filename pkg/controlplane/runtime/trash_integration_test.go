package runtime

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestRuntimeTrash_RecycleListEmpty_EndToEnd exercises the full T7b wiring
// against a REAL Runtime built over a real local-fs CAS block store:
//
//  1. The metadata.TrashPolicy installed in New() is live, so a RemoveFile on a
//     trash-enabled share RECYCLES (moves to #recycle) instead of destroying.
//  2. rt.Trash().List surfaces the recycled root through the real Deps adapter
//     (MetadataServiceForShare resolves the runtime's shared metadata service +
//     the share root handle).
//  3. rt.Trash().Empty permanently purges the bin and frees the file's CAS
//     blocks through the REAL FreeBlocks path (GetBlockStoreForHandle +
//     engine.Store.Delete) — not a stub.
func TestRuntimeTrash_RecycleListEmpty_EndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fx := newByteVerifyFixture(t, metadatamemory.NewMemoryMetadataStoreWithDefaults(), "memory")
	t.Cleanup(fx.close)

	// Enable trash on the share (runtime-only; the recycle decision reads the
	// runtime registry via TrashSettingsForShare). Nil store keeps the DB out
	// of this unit test.
	if err := fx.rt.sharesSvc.SetShareTrashConfig(nil, fx.shareName, shares.TrashSettings{
		Enabled: true,
	}); err != nil {
		t.Fatalf("SetShareTrashConfig: %v", err)
	}

	// Write a real file with CAS-backed content.
	const fileName = "doc.txt"
	payloadID := fx.createEmptyFile(ctx, fileName)
	data := bytes.Repeat([]byte("trash-me"), 4096) // 32 KiB, multiple chunks
	fx.writeFile(ctx, payloadID, data)

	// Content is readable before recycle (sanity: the bytes are really in CAS).
	res, err := common.ReadFromBlockStore(ctx, fx.bs, payloadID, 0, uint32(len(data)))
	if err != nil {
		t.Fatalf("ReadFromBlockStore (pre-recycle): %v", err)
	}
	if !bytes.Equal(res.Data, data) {
		t.Fatalf("pre-recycle content mismatch: got %d bytes", len(res.Data))
	}

	root, err := fx.rt.GetRootHandle(fx.shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	actx := metadata.NewSystemAuthContext(ctx)
	svc := fx.rt.GetMetadataService()

	// RemoveFile through the metadata service: the installed TrashPolicy must
	// RECYCLE (not destroy). The returned File carries an empty PayloadID
	// because the content was moved, not deleted.
	removed, err := svc.RemoveFile(actx, root, fileName)
	if err != nil {
		t.Fatalf("RemoveFile (recycle): %v", err)
	}
	if removed.PayloadID != "" {
		t.Fatalf("recycle should not destroy content: got PayloadID %q", removed.PayloadID)
	}

	// The original name must be gone from the share root...
	if _, err := svc.GetChild(ctx, root, fileName); !metadata.IsNotFoundError(err) {
		t.Fatalf("expected %q gone from root after recycle, got err=%v", fileName, err)
	}
	// ...and the CAS bytes must still exist (recycle is non-destructive).
	if _, err := common.ReadFromBlockStore(ctx, fx.bs, payloadID, 0, uint32(len(data))); err != nil {
		t.Fatalf("content should survive recycle: %v", err)
	}

	// List must surface exactly one recycled root via the real Deps adapter.
	entries, err := fx.rt.Trash().List(actx, fx.shareName)
	if err != nil {
		t.Fatalf("Trash().List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 recycled entry, got %d: %+v", len(entries), entries)
	}
	if entries[0].OriginalPath != fileName && entries[0].OriginalPath != "/"+fileName {
		t.Fatalf("unexpected OriginalPath %q", entries[0].OriginalPath)
	}
	if entries[0].IsDir {
		t.Fatalf("recycled file reported as directory")
	}

	// Empty must purge the entry and free its CAS blocks via the REAL
	// FreeBlocks path (GetBlockStoreForHandle + engine.Store.Delete).
	n, err := fx.rt.Trash().Empty(actx, fx.shareName, false)
	if err != nil {
		t.Fatalf("Trash().Empty: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected Empty to remove 1 entry, got %d", n)
	}

	// The bin is now empty.
	after, err := fx.rt.Trash().List(actx, fx.shareName)
	if err != nil {
		t.Fatalf("Trash().List (post-empty): %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("expected empty bin after Empty, got %d entries", len(after))
	}

	// Empty (above) already drove FreeBlocks through the REAL Deps adapter
	// (GetBlockStoreForHandle + engine.Store.Delete) with the recycled file's
	// BlockRef list. Re-invoke the adapter directly here as a wiring smoke test:
	// it must resolve the per-share store and return cleanly. The payload was
	// already purged by Empty, so a nil block list suffices to prove the path
	// resolves (unit coverage of block-threading lives in the trash package's
	// TestEmptyThreadsBlocksToFreeBlocks).
	d := &trashDeps{rt: fx.rt}
	if err := d.FreeBlocks(ctx, fx.shareName, root, string(payloadID), nil); err != nil {
		t.Fatalf("FreeBlocks (real GetBlockStoreForHandle+Delete path): %v", err)
	}
	// And an empty payloadID is a no-op (no store resolution, no error).
	if err := d.FreeBlocks(ctx, fx.shareName, root, "", nil); err != nil {
		t.Fatalf("FreeBlocks(empty payload) should be a no-op: %v", err)
	}
}
