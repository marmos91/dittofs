// Handler-level coverage for the per-share AllowMFsymlink gate and the
// MFsymlink-target path-traversal guard on CLOSE.
//
// macOS/Windows SMB clients create symlinks by writing 1067-byte MFsymlink
// (XSym) content. On CLOSE, DittoFS optionally promotes such a file to a real
// symlink for NFS interoperability. The promotion target is fully
// client-controlled, so:
//   - it is opt-in per share (AllowMFsymlink, default false), and
//   - the decoded target is rejected if it is absolute or contains `..`
//     (would escape the share root).
//
// These tests drive h.Close end-to-end against a memory metadata + block store,
// the same backends every smb-conformance profile uses.
package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupMFsymlinkShare wires a handler + runtime + memory metadata + memory
// block store with a single share whose AllowMFsymlink flag is set per the
// argument. It then creates a regular file containing the given MFsymlink
// target's XSym payload and registers an OpenFile for it. Returns the handler,
// a primed SMBHandlerContext, the share root handle, and the FileID of the
// registered OpenFile. The created entry is named "link" under the root.
func setupMFsymlinkShare(t *testing.T, allowMFsymlink bool, target string) (*Handler, *SMBHandlerContext, metadata.FileHandle, [16]byte) {
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

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "mfmeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	metaStore := metamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("mfmeta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	// A plain memory local store suffices: require_durable_commit defaults to
	// false, so CLOSE acks once the flush succeeds regardless of durability
	// (#1274). This test exercises MFsymlink conversion, not durability.
	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "mfbs", Kind: models.BlockStoreKindLocal, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/mf"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "mfmeta",
		Enabled:           true,
		LocalBlockStoreID: localBSID,
		AllowMFsymlink:    allowMFsymlink,
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

	const fileName = "link"
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

	// Build and write the XSym MFsymlink payload (exactly mfsymlink.Size bytes).
	payload, err := mfsymlink.Encode(target)
	if err != nil {
		t.Fatalf("mfsymlink.Encode(%q): %v", target, err)
	}
	if len(payload) != mfsymlink.Size {
		t.Fatalf("encoded MFsymlink is %d bytes, expected %d", len(payload), mfsymlink.Size)
	}

	bs, err := rt.GetBlockStoreForHandle(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetBlockStoreForHandle: %v", err)
	}
	writeOp, err := metaSvc.PrepareWrite(authCtx, fileHandle, uint64(len(payload)))
	if err != nil {
		t.Fatalf("PrepareWrite: %v", err)
	}
	if _, err := bs.WriteAt(ctx, string(writeOp.PayloadID), nil, payload, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := metaSvc.CommitWrite(authCtx, writeOp); err != nil {
		t.Fatalf("CommitWrite: %v", err)
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Re-fetch to obtain the committed PayloadID + size.
	committed, err := metaSvc.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile post-write: %v", err)
	}
	if committed.Size != mfsymlink.Size {
		t.Fatalf("committed size = %d, expected %d", committed.Size, mfsymlink.Size)
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
		TreeID:         treeID,
		SessionID:      sess.SessionID,
		ShareName:      shareName,
		Permission:     models.PermissionReadWrite,
		AllowMFsymlink: allowMFsymlink,
	})

	fileID := [16]byte{1}
	openFile := &OpenFile{
		FileID:         fileID,
		TreeID:         treeID,
		SessionID:      sess.SessionID,
		Path:           fileName,
		ShareName:      shareName,
		DesiredAccess:  uint32(types.FileReadData | types.FileWriteData),
		GrantedAccess:  uint32(types.FileReadData | types.FileWriteData),
		MetadataHandle: fileHandle,
		PayloadID:      committed.PayloadID,
		ParentHandle:   rootHandle,
		FileName:       fileName,
	}
	h.StoreOpenFile(openFile)

	smbCtx := &SMBHandlerContext{
		Context:   ctx,
		SessionID: sess.SessionID,
		TreeID:    treeID,
		ShareName: shareName,
	}

	return h, smbCtx, rootHandle, fileID
}

// fileTypeAfterClose closes the registered FileID and reports the metadata file
// type of the "link" entry under rootHandle. MFsymlink conversion removes the
// regular file and re-creates the entry as a symlink under the same name, so
// the entry must always be resolvable by name from its parent.
func fileTypeAfterClose(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, rootHandle metadata.FileHandle, fileID [16]byte) metadata.FileType {
	t.Helper()
	resp, err := h.Close(smbCtx, &CloseRequest{FileID: fileID})
	if err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("Close: status = 0x%08x, expected STATUS_SUCCESS", uint32(resp.Status))
	}
	metaSvc := h.Registry.GetMetadataService()
	childHandle, err := metaSvc.GetChild(smbCtx.Context, rootHandle, "link")
	if err != nil {
		t.Fatalf("GetChild(link) after close: %v", err)
	}
	file, err := metaSvc.GetFile(smbCtx.Context, childHandle)
	if err != nil {
		t.Fatalf("GetFile after close: %v", err)
	}
	return file.Type
}

// TestClose_MFsymlink_DisabledByDefault confirms an XSym file is NOT promoted
// to a symlink when the share's AllowMFsymlink toggle is false (the default).
func TestClose_MFsymlink_DisabledByDefault(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupMFsymlinkShare(t, false, "valid/target")
	if got := fileTypeAfterClose(t, h, smbCtx, rootHandle, fileID); got != metadata.FileTypeRegular {
		t.Fatalf("file type after close = %v, expected FileTypeRegular (conversion fired with AllowMFsymlink=false)", got)
	}
}

// TestClose_MFsymlink_EnabledConverts confirms an XSym file IS promoted to a
// real symlink when AllowMFsymlink is true and the target is a safe relative
// path.
func TestClose_MFsymlink_EnabledConverts(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupMFsymlinkShare(t, true, "valid/target")
	if got := fileTypeAfterClose(t, h, smbCtx, rootHandle, fileID); got != metadata.FileTypeSymlink {
		t.Fatalf("file type after close = %v, expected FileTypeSymlink (conversion did not fire with AllowMFsymlink=true)", got)
	}
}

// TestClose_MFsymlink_TraversalTargetRejected confirms that even with
// AllowMFsymlink=true, a traversal target (`..`) is rejected and the file is
// left as a regular file — no symlink escaping the share root is created.
func TestClose_MFsymlink_TraversalTargetRejected(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupMFsymlinkShare(t, true, "../../etc/passwd")
	if got := fileTypeAfterClose(t, h, smbCtx, rootHandle, fileID); got != metadata.FileTypeRegular {
		t.Fatalf("file type after close = %v, expected FileTypeRegular (traversal target was promoted to a symlink)", got)
	}
}
