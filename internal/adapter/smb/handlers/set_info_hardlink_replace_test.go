package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

const hardlinkTestShareName = "/hl"

// newHardlinkTestShare builds a memory-backed runtime holding one empty share
// and returns it with that share's root handle and a root auth context. Tests
// that need a plain context for block-store calls take authCtx.Context.
func newHardlinkTestShare(t *testing.T) (*runtime.Runtime, metadata.FileHandle, *metadata.AuthContext) {
	t.Helper()
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	rt := runtime.New(cps)

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "hlmeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore("hlmeta", metamemory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "hlbs", Kind: models.BlockStoreKindLocal, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              hardlinkTestShareName,
		MetadataStore:     "hlmeta",
		Enabled:           true,
		LocalBlockStoreID: localBSID,
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(hardlinkTestShareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	uid, gid := uint32(0), uint32(0)
	return rt, rootHandle, &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
}

// TestSetInfo_HardlinkReplace_ReclaimsReplacedPayload pins that a SET_INFO
// FileLinkInformation with ReplaceIfExists=TRUE frees the content of the file it
// replaced, not just its directory entry. The metadata layer deliberately never
// deletes payload bytes — RemoveFile returns the removed file's PayloadID so the
// handler can — and a handler that ignores that return leaves the replaced
// file's records indexed as live in the local tier, where no reclamation path
// treats them as dead and the bytes survive every restart.
//
// Asserting on observable block-store state pins both directions: the replaced
// file's payload must be gone AND the newly linked file's payload must survive,
// so releasing the wrong payload fails here instead of passing.
func TestSetInfo_HardlinkReplace_ReclaimsReplacedPayload(t *testing.T) {
	rt, rootHandle, authCtx := newHardlinkTestShare(t)
	ctx := authCtx.Context
	metaSvc := rt.GetMetadataService()

	// create makes a file holding `size` bytes of a non-zero pattern and
	// returns its handle and payload id.
	create := func(name string, size int) (metadata.FileHandle, string) {
		t.Helper()
		file, _, err := metaSvc.CreateFile(authCtx, rootHandle, name, &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644,
		})
		if err != nil {
			t.Fatalf("CreateFile %s: %v", name, err)
		}
		handle, err := metadata.EncodeFileHandle(file)
		if err != nil {
			t.Fatalf("EncodeFileHandle %s: %v", name, err)
		}
		bs, err := rt.GetBlockStoreForHandle(ctx, handle)
		if err != nil {
			t.Fatalf("GetBlockStoreForHandle %s: %v", name, err)
		}
		wop, err := metaSvc.PrepareWrite(authCtx, handle, uint64(size))
		if err != nil {
			t.Fatalf("PrepareWrite %s: %v", name, err)
		}
		data := make([]byte, size)
		for i := range data {
			data[i] = byte((i % 251) + 1)
		}
		if _, err := bs.WriteAt(ctx, string(wop.PayloadID), nil, data, 0); err != nil {
			t.Fatalf("WriteAt %s: %v", name, err)
		}
		if _, err := metaSvc.CommitWrite(authCtx, wop); err != nil {
			t.Fatalf("CommitWrite %s: %v", name, err)
		}
		if _, err := metaSvc.FlushPendingWriteForFile(authCtx, handle, true); err != nil {
			t.Fatalf("Flush %s: %v", name, err)
		}
		return handle, string(wop.PayloadID)
	}

	// The file that ReplaceIfExists will destroy, and the file being linked
	// over it. Distinct sizes so a mixed-up payload id cannot pass unnoticed.
	_, victimPayload := create("victim.bin", 4096)
	srcHandle, srcPayload := create("src.bin", 2048)

	// Without this the two Exists assertions below could both be reading the
	// same payload, and the test would pass whatever the handler did.
	if victimPayload == srcPayload {
		t.Fatalf("test setup: both files share payload %q, the assertions below cannot discriminate", victimPayload)
	}

	bs, err := rt.GetBlockStoreForHandle(ctx, srcHandle)
	if err != nil {
		t.Fatalf("GetBlockStoreForHandle: %v", err)
	}
	exists := func(payloadID string) bool {
		t.Helper()
		ok, err := bs.Exists(ctx, payloadID)
		if err != nil {
			t.Fatalf("Exists %s: %v", payloadID, err)
		}
		return ok
	}
	if !exists(victimPayload) {
		t.Fatalf("setup broken: victim payload %q absent before the hardlink replace", victimPayload)
	}

	h := NewHandler()
	h.Registry = rt
	// A zero RootDirectory means the link name is resolved from the share
	// root, which the handler reaches through the tree.
	const treeID = uint32(7)
	h.StoreTree(&TreeConnection{TreeID: treeID, ShareName: hardlinkTestShareName})
	open := (&OpenFile{
		FileID:         [16]byte{0x2C, 0x71, 0x04, 0x18},
		MetadataHandle: srcHandle,
		ShareName:      hardlinkTestShareName,
		TreeID:         treeID,
		DesiredAccess:  uint32(types.FileWriteAttributes),
	}).WithName(OpenName{Path: "src.bin", FileName: "src.bin", ParentHandle: rootHandle})
	h.StoreOpenFile(open)

	// Hard-link src.bin onto the existing name victim.bin, replacing it.
	buf := encodeFileLinkInfoWire(t, true, [8]byte{}, "victim.bin")
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileLinkInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore(FileLinkInformation): err=%v resp=%v", err, resp)
	}

	if exists(victimPayload) {
		size, _ := bs.GetSize(ctx, victimPayload)
		t.Errorf("replaced payload %q still holds %d live bytes after the hardlink replace", victimPayload, size)
	}
	if !exists(srcPayload) {
		t.Errorf("linked file's payload %q was dropped by the hardlink replace", srcPayload)
	}
}
