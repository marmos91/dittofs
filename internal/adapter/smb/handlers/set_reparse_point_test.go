// Handler-level coverage for native SMB symlink creation via
// FSCTL_SET_REPARSE_POINT (#1179). macOS and Windows create symlinks over SMB
// by issuing this FSCTL on a freshly-created file; the handler converts the
// placeholder into a real metadata symlink. These tests drive
// h.handleSetReparsePoint end-to-end against a memory metadata + block store.
package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupReparseShare wires a handler + runtime + memory metadata + memory block
// store with a single share, creates an empty regular file "link" under the
// root (the placeholder a client opens before SET_REPARSE_POINT), and registers
// an OpenFile for it. Returns the handler, a primed SMBHandlerContext, the share
// root handle, and the FileID of the registered OpenFile.
func setupReparseShare(t *testing.T) (*Handler, *SMBHandlerContext, metadata.FileHandle, [16]byte) {
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

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "rpmeta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	metaStore := metamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("rpmeta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "rpbs", Kind: models.BlockStoreKindLocal, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/rp"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "rpmeta",
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

// buildSetReparseBody assembles a minimal FSCTL_SET_REPARSE_POINT IOCTL request
// body [MS-SMB2] 2.2.31 carrying the given REPARSE_DATA_BUFFER as input. The
// 56-byte fixed portion is followed by the buffer; InputOffset is the
// SMB2-header-relative offset (64 + 56).
func buildSetReparseBody(fileID [16]byte, reparse []byte) []byte {
	w := smbenc.NewWriter(56 + len(reparse))
	w.WriteUint16(57)                   // StructureSize
	w.WriteUint16(0)                    // Reserved
	w.WriteUint32(FsctlSetReparsePoint) // CtlCode
	w.WriteBytes(fileID[:])             // FileId
	w.WriteUint32(120)                  // InputOffset (header 64 + fixed body 56)
	w.WriteUint32(uint32(len(reparse))) // InputCount
	w.WriteUint32(0)                    // MaxInputResponse
	w.WriteUint32(0)                    // OutputOffset
	w.WriteUint32(0)                    // OutputCount
	w.WriteUint32(0)                    // MaxOutputResponse
	w.WriteUint32(0)                    // Flags
	w.WriteUint32(0)                    // Reserved2
	w.WriteBytes(reparse)
	return w.Bytes()
}

// buildNfsSymlinkReparseBuffer builds an IO_REPARSE_TAG_NFS REPARSE_DATA_BUFFER
// carrying an NFS_SPECFILE_LNK symlink target (UTF-8), as Linux cifs.ko emits
// with mount option reparse=nfs.
func buildNfsSymlinkReparseBuffer(target string) []byte {
	data := smbenc.NewWriter(8 + len(target))
	data.WriteUint64(nfsSpecfileLnk)
	data.WriteBytes([]byte(target))
	dataBytes := data.Bytes()

	w := smbenc.NewWriter(8 + len(dataBytes))
	w.WriteUint32(IoReparseTagNfs)
	w.WriteUint16(uint16(len(dataBytes)))
	w.WriteUint16(0) // Reserved
	w.WriteBytes(dataBytes)
	return w.Bytes()
}

// symlinkTargetAfter resolves the "link" entry under rootHandle and returns its
// metadata type and (if a symlink) its target.
func symlinkTargetAfter(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, rootHandle metadata.FileHandle) (metadata.FileType, string) {
	t.Helper()
	metaSvc := h.Registry.GetMetadataService()
	childHandle, err := metaSvc.GetChild(smbCtx.Context, rootHandle, "link")
	if err != nil {
		t.Fatalf("GetChild(link): %v", err)
	}
	file, err := metaSvc.GetFile(smbCtx.Context, childHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	return file.Type, file.LinkTarget
}

func TestSetReparsePoint_CreatesRelativeSymlink(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupReparseShare(t)
	body := buildSetReparseBody(fileID, buildSymlinkReparseBuffer("../A"))

	resp, err := h.handleSetReparsePoint(smbCtx, body)
	if err != nil {
		t.Fatalf("handleSetReparsePoint: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%08x, want STATUS_SUCCESS", uint32(resp.Status))
	}

	gotType, gotTarget := symlinkTargetAfter(t, h, smbCtx, rootHandle)
	if gotType != metadata.FileTypeSymlink {
		t.Fatalf("type = %v, want FileTypeSymlink", gotType)
	}
	if gotTarget != "../A" {
		t.Fatalf("target = %q, want %q", gotTarget, "../A")
	}
}

// TestSetReparsePoint_AbsoluteTargetAllowed documents the permissive (NFS-style)
// validation decision: absolute targets are accepted, matching what the NFS
// adapter already stores (targets are opaque, client-resolved strings).
func TestSetReparsePoint_AbsoluteTargetAllowed(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupReparseShare(t)
	body := buildSetReparseBody(fileID, buildSymlinkReparseBuffer("/usr/local/lib/foo"))

	resp, err := h.handleSetReparsePoint(smbCtx, body)
	if err != nil {
		t.Fatalf("handleSetReparsePoint: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%08x, want STATUS_SUCCESS", uint32(resp.Status))
	}
	gotType, gotTarget := symlinkTargetAfter(t, h, smbCtx, rootHandle)
	if gotType != metadata.FileTypeSymlink || gotTarget != "/usr/local/lib/foo" {
		t.Fatalf("got (%v, %q), want (symlink, %q)", gotType, gotTarget, "/usr/local/lib/foo")
	}
}

// TestSetReparsePoint_CloseAfterConvertIsNoop verifies the stale-handle refresh:
// after conversion the still-open handle is repointed at the new symlink so the
// trailing CLOSE succeeds and the symlink survives.
func TestSetReparsePoint_CloseAfterConvertIsNoop(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupReparseShare(t)
	body := buildSetReparseBody(fileID, buildSymlinkReparseBuffer("target/path"))

	if _, err := h.handleSetReparsePoint(smbCtx, body); err != nil {
		t.Fatalf("handleSetReparsePoint: %v", err)
	}

	resp, err := h.Close(smbCtx, &CloseRequest{FileID: fileID})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("Close status = 0x%08x, want STATUS_SUCCESS", uint32(resp.Status))
	}

	gotType, gotTarget := symlinkTargetAfter(t, h, smbCtx, rootHandle)
	if gotType != metadata.FileTypeSymlink || gotTarget != "target/path" {
		t.Fatalf("after close got (%v, %q), want (symlink, %q)", gotType, gotTarget, "target/path")
	}
}

func TestSetReparsePoint_WrongTagRejected(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)

	// REPARSE_DATA_BUFFER with a non-symlink tag (mount point).
	const ioReparseTagMountPoint uint32 = 0xA0000003
	w := smbenc.NewWriter(16)
	w.WriteUint32(ioReparseTagMountPoint)
	w.WriteUint16(8) // ReparseDataLength
	w.WriteUint16(0) // Reserved
	w.WriteUint64(0) // bogus data
	body := buildSetReparseBody(fileID, w.Bytes())

	resp, err := h.handleSetReparsePoint(smbCtx, body)
	if err != nil {
		t.Fatalf("handleSetReparsePoint: %v", err)
	}
	if resp.Status != types.StatusIoReparseTagMismatch {
		t.Fatalf("status = 0x%08x, want STATUS_IO_REPARSE_TAG_MISMATCH", uint32(resp.Status))
	}
}

func TestSetReparsePoint_TruncatedBufferRejected(t *testing.T) {
	h, smbCtx, _, fileID := setupReparseShare(t)
	body := buildSetReparseBody(fileID, []byte{0x0c, 0x00, 0x00}) // < 8 bytes

	resp, err := h.handleSetReparsePoint(smbCtx, body)
	if err != nil {
		t.Fatalf("handleSetReparsePoint: %v", err)
	}
	if resp.Status != types.StatusInvalidParameter {
		t.Fatalf("status = 0x%08x, want STATUS_INVALID_PARAMETER", uint32(resp.Status))
	}
}

func TestSetReparsePoint_NfsTagSymlink(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupReparseShare(t)
	body := buildSetReparseBody(fileID, buildNfsSymlinkReparseBuffer("../sibling"))

	resp, err := h.handleSetReparsePoint(smbCtx, body)
	if err != nil {
		t.Fatalf("handleSetReparsePoint: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%08x, want STATUS_SUCCESS", uint32(resp.Status))
	}
	gotType, gotTarget := symlinkTargetAfter(t, h, smbCtx, rootHandle)
	if gotType != metadata.FileTypeSymlink || gotTarget != "../sibling" {
		t.Fatalf("got (%v, %q), want (symlink, %q)", gotType, gotTarget, "../sibling")
	}
}

func TestSetReparsePoint_DispatchedViaIoctl(t *testing.T) {
	h, smbCtx, rootHandle, fileID := setupReparseShare(t)
	body := buildSetReparseBody(fileID, buildSymlinkReparseBuffer("dispatched"))

	resp, err := h.Ioctl(smbCtx, body)
	if err != nil {
		t.Fatalf("Ioctl: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("Ioctl status = 0x%08x, want STATUS_SUCCESS", uint32(resp.Status))
	}
	if gotType, _ := symlinkTargetAfter(t, h, smbCtx, rootHandle); gotType != metadata.FileTypeSymlink {
		t.Fatalf("type = %v, want FileTypeSymlink", gotType)
	}
}

func TestBuildAAPLServerQueryResponse(t *testing.T) {
	// Server query (command code 1): expect a 24-byte reply advertising
	// UNIX-based server caps.
	req := smbenc.NewWriter(24)
	req.WriteUint32(aaplCommandServerQuery)
	req.WriteUint32(0)
	req.WriteUint64(0x7) // RequestBitmap: caps|volcaps|model
	req.WriteUint64(0)   // ClientCapabilities

	resp := buildAAPLServerQueryResponse(req.Bytes())
	if len(resp) != 24 {
		t.Fatalf("response len = %d, want 24", len(resp))
	}
	r := smbenc.NewReader(resp)
	if cmd := r.ReadUint32(); cmd != aaplCommandServerQuery {
		t.Fatalf("CommandCode = %d, want %d", cmd, aaplCommandServerQuery)
	}
	r.Skip(4) // Reserved
	if bitmap := r.ReadUint64(); bitmap != aaplReplyBitmapServerCaps {
		t.Fatalf("ReplyBitmap = 0x%x, want 0x%x", bitmap, aaplReplyBitmapServerCaps)
	}
	if caps := r.ReadUint64(); caps&aaplServerCapUnixBased == 0 {
		t.Fatalf("ServerCaps = 0x%x, missing UNIX_BASED bit", caps)
	}

	// Non-server-query command code → no response.
	other := smbenc.NewWriter(8)
	other.WriteUint32(99)
	other.WriteUint32(0)
	if got := buildAAPLServerQueryResponse(other.Bytes()); got != nil {
		t.Fatalf("non-query command returned %d bytes, want nil", len(got))
	}

	// Too-short data → no response.
	if got := buildAAPLServerQueryResponse([]byte{1, 0, 0}); got != nil {
		t.Fatalf("short data returned %d bytes, want nil", len(got))
	}
}
