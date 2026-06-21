// Regression coverage for #1267: SMB CLOSE must surface a block-store flush
// failure instead of silently acknowledging the close with STATUS_SUCCESS.
//
// CLOSE is a durability point (MS-SMB2 3.3.5.10): the client treats a
// successful CLOSE as proof that its writes reached stable storage. Before the
// fix, Close() logged the flush error at WARN and returned StatusSuccess
// anyway, so a payload that never durably flushed (e.g. append-log
// backpressure surfacing fs.ErrPressureTimeout, or the share's block store
// going away mid-close → engine.ErrStoreClosed) was reported as committed —
// silent write truncation.
//
// The handler-level test drives h.Close end-to-end against a memory metadata +
// block store (the same backends every smb-conformance profile uses) but
// closes the underlying engine.Store first, so the flush deterministically
// returns engine.ErrStoreClosed. That sentinel maps to STATUS_FILE_CLOSED via
// common.MapContentToSMB — any non-success status proves the swallow is fixed.
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

// setupFlushErrorShare wires a handler + runtime + memory metadata + memory
// block store with a single read-write share, writes a small payload to a file
// named "data" under the share root, and registers an OpenFile for it. Returns
// the handler, a primed SMBHandlerContext, the file's metadata handle, and the
// FileID of the registered OpenFile.
func setupFlushErrorShare(t *testing.T) (*Handler, *SMBHandlerContext, metadata.FileHandle, [16]byte) {
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

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "flushmeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	metaStore := metamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("flushmeta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "flushbs", Kind: models.BlockStoreKindLocal, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/flush"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "flushmeta",
		Enabled:           true,
		LocalBlockStoreID: localBSID,
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

	return h, smbCtx, fileHandle, fileID
}

// TestClose_FlushFailure_SurfacedAsNonSuccess proves that when the block-store
// flush fails during CLOSE, the handler returns a non-success NTSTATUS rather
// than the pre-#1267 silent StatusSuccess. The flush is forced to fail by
// closing the underlying engine.Store first, so engine.Flush returns
// engine.ErrStoreClosed (the same mapping seam fs.ErrPressureTimeout and the
// other block-store content errors travel through).
func TestClose_FlushFailure_SurfacedAsNonSuccess(t *testing.T) {
	h, smbCtx, fileHandle, fileID := setupFlushErrorShare(t)

	// Force the durable flush to fail: close the share's block store so the
	// CLOSE-time engine.Flush short-circuits with engine.ErrStoreClosed.
	bs, err := h.Registry.GetBlockStoreForHandle(smbCtx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetBlockStoreForHandle: %v", err)
	}
	if err := bs.Close(); err != nil {
		t.Fatalf("close block store: %v", err)
	}

	resp, err := h.Close(smbCtx, &CloseRequest{FileID: fileID})
	if err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("Close: nil response")
	}
	if resp.Status == types.StatusSuccess {
		t.Fatalf("Close: status = STATUS_SUCCESS, but the durable flush failed — "+
			"#1267 regression: a non-flushed payload was acknowledged as committed (resp=%+v)", resp)
	}
	// engine.ErrStoreClosed maps to STATUS_FILE_CLOSED via MapContentToSMB; pin
	// it so the test also guards the chosen mapping, not merely "any non-zero".
	if resp.Status != types.StatusFileClosed {
		t.Fatalf("Close: status = 0x%08x, expected STATUS_FILE_CLOSED (0x%08x) for a closed-store flush failure",
			uint32(resp.Status), uint32(types.StatusFileClosed))
	}

	// The handle must still be released even though the flush failed — a failed
	// durability point must not leak the open-file entry.
	if _, ok := h.GetOpenFile(fileID); ok {
		t.Fatal("Close: open file handle still registered after CLOSE; failed flush leaked the handle")
	}
}
