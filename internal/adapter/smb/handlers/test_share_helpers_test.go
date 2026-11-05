package handlers

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/smb/types"
	"github.com/cubbit/dittofs/pkg/controlplane/models"
	"github.com/cubbit/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/cubbit/dittofs/pkg/controlplane/store"
	"github.com/cubbit/dittofs/pkg/metadata"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

const hardlinkTestShareName = "/hl"

// newHardlinkTestShare builds a memory-backed runtime holding one empty share
// and returns it with that share's root handle and a root auth context.
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
	rt := newTestRuntime(t, cps)

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "hlmeta", Type: "badger", Config: `{"in_memory":true}`}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore("hlmeta", badgertest.NewInMemory(t)); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "hlbs", Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:          hardlinkTestShareName,
		MetadataStore: "hlmeta",
		Enabled:       true,
		BlockStoreID:  localBSID,
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
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

// createHardlinkTestFile creates name under parent with a committed size of
// size bytes, and returns the file's handle with the payload id backing its
// content. No content store is wired, so the bytes themselves are not stored.
func createHardlinkTestFile(
	t *testing.T,
	rt *runtime.Runtime,
	authCtx *metadata.AuthContext,
	parent metadata.FileHandle,
	name string,
	size int,
) (metadata.FileHandle, string) {
	t.Helper()
	metaSvc := rt.GetMetadataService()

	file, _, err := metaSvc.CreateFile(authCtx, parent, name, &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile %s: %v", name, err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle %s: %v", name, err)
	}
	wop, err := metaSvc.PrepareWrite(authCtx, handle, uint64(size))
	if err != nil {
		t.Fatalf("PrepareWrite %s: %v", name, err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, wop); err != nil {
		t.Fatalf("CommitWrite %s: %v", name, err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, handle, true); err != nil {
		t.Fatalf("Flush %s: %v", name, err)
	}
	return handle, string(wop.PayloadID)
}

// openHardlinkTestFile returns a handler carrying one tree and one open handle
// on name, which is everything setFileInfoFromStore needs to serve a link
// request: a zero RootDirectory makes it resolve the link name from the share
// root, and it reaches that root through the tree.
func openHardlinkTestFile(
	t *testing.T,
	rt *runtime.Runtime,
	parent metadata.FileHandle,
	handle metadata.FileHandle,
	name string,
) (*Handler, *OpenFile) {
	t.Helper()
	const treeID = uint32(7)

	h := NewHandler()
	h.Registry = rt
	h.StoreTree(&TreeConnection{TreeID: treeID, ShareName: hardlinkTestShareName})
	open := (&OpenFile{
		FileID:         [16]byte{0x2C, 0x71, 0x04, 0x18},
		MetadataHandle: handle,
		ShareName:      hardlinkTestShareName,
		TreeID:         treeID,
		DesiredAccess:  uint32(types.FileWriteAttributes),
	}).WithName(OpenName{Path: name, FileName: name, ParentHandle: parent})
	h.StoreOpenFile(open)
	return h, open
}

// setupWriteTestShare wires a handler + runtime + the caller's metadata store
// with a single read-write share, commits a small write's size to a file named
// "data" under the share root (no content store is wired, so no bytes are
// stored), and registers an OpenFile for it. A
// nil metaStore uses a plain memory metadata store. Returns the handler, a
// primed SMBHandlerContext, the file's metadata handle, and the FileID of the
// registered OpenFile.
func setupWriteTestShare(t *testing.T, metaStore metadata.Store) (*Handler, *SMBHandlerContext, metadata.FileHandle, [16]byte) {
	t.Helper()
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	rt := newTestRuntime(t, cps)

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "flushmeta", Type: "badger", Config: `{"in_memory":true}`}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if metaStore == nil {
		metaStore = badgertest.NewInMemory(t)
	}
	if err := rt.RegisterMetadataStore("flushmeta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "flushbs", Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/flush"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "flushmeta",
		Enabled:       true,
		BlockStoreID:  localBSID,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	metaSvc := rt.GetMetadataService()

	const fileName = "data"
	file, _, err := metaSvc.CreateFile(authCtx, rootHandle, fileName, &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	payload := []byte("payload-that-must-flush")
	writeOp, err := metaSvc.PrepareWrite(authCtx, fileHandle, uint64(len(payload)))
	if err != nil {
		t.Fatalf("PrepareWrite: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, writeOp); err != nil {
		t.Fatalf("CommitWrite: %v", err)
	}

	committed, err := metaSvc.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile post-write: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	const callerUID, callerGID uint32 = 1000, 1000
	suid, sgid := callerUID, callerGID
	sess := h.CreateSession("127.0.0.1:12345", false, "test-user", "")
	sess.User = &models.User{
		Username: "test-user",
		UID:      &suid,
		Groups:   []models.Group{{GID: &sgid}},
	}

	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{
		TreeID:     treeID,
		SessionID:  sess.SessionID,
		ShareName:  shareName,
		Permission: models.PermissionReadWrite,
	})

	fileID := [16]byte{7}
	openFile := (&OpenFile{
		FileID:         fileID,
		TreeID:         treeID,
		SessionID:      sess.SessionID,
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileReadData | types.FileWriteData),
		GrantedAccess:  uint32(types.FileReadData | types.FileWriteData),
		MetadataHandle: fileHandle,
		PayloadID:      committed.PayloadID,
	}).WithName(OpenName{Path: fileName, FileName: fileName, ParentHandle: rootHandle})
	h.StoreOpenFile(openFile)

	smbCtx := &SMBHandlerContext{
		Context:   ctx,
		SessionID: sess.SessionID,
		TreeID:    treeID,
		ShareName: shareName,
	}

	return h, smbCtx, fileHandle, fileID
}

// TestSetInfo_HardlinkReplace_SelfLinkKeepsContent pins that linking a file
// onto the name it already has is a no-op rather than a self-destruct.
//
// ReplaceIfExists removes the existing destination before creating the link. If
// that destination IS the file being linked, and it holds the only link, the
// removal drops the last link, and CreateHardLink then resurrects the name over
// a different inode. The name must keep resolving to the same file, with its
// size and the payload behind it.
func TestSetInfo_HardlinkReplace_SelfLinkKeepsContent(t *testing.T) {
	rt, rootHandle, authCtx := newHardlinkTestShare(t)
	ctx := authCtx.Context

	handle, payloadID := createHardlinkTestFile(t, rt, authCtx, rootHandle, "a.txt", 4096)
	h, open := openHardlinkTestFile(t, rt, rootHandle, handle, "a.txt")

	// Link a.txt onto its own name with ReplaceIfExists set.
	buf := encodeFileLinkInfoWire(t, true, [8]byte{}, "a.txt")
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileLinkInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore(self link): err=%v resp=%v", err, resp)
	}

	child, cErr := rt.GetMetadataService().GetChild(ctx, rootHandle, "a.txt")
	if cErr != nil {
		t.Fatalf("a.txt no longer resolves after linking it onto its own name: %v", cErr)
	}
	file, err := rt.GetMetadataService().GetFile(ctx, child)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if string(file.PayloadID) != payloadID || file.Size != 4096 {
		t.Errorf("a.txt after a self link = payload %q size %d, want payload %q size 4096", file.PayloadID, file.Size, payloadID)
	}
}
