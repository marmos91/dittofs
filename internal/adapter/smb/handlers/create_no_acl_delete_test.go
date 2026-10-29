package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// A file with no stored ACL is gated at open by its POSIX mode, owner and
// parent. User B holds a read-write share grant but cannot write user A's
// directory, so B must not be able to delete A's 0644 file through either
// delete-on-close route, while A still can.

const (
	noACLOwnerUID uint32 = 1000 // user A, set up by setupStreamsDisabledShare
	noACLOtherUID uint32 = 2000 // user B
)

// setupNoACLDeleteShare returns a handler with two sessions on one share —
// A's (uid 1000) and B's (uid 2000), both with a read-write tree — and A's
// directory "adir" (0755) holding A's file "adir/a.txt" (0644), neither with
// a stored ACL.
func setupNoACLDeleteShare(t *testing.T) (h *Handler, ctxA, ctxB *SMBHandlerContext, dir metadata.FileHandle) {
	t.Helper()
	h, ctxA, root := setupStreamsDisabledShare(t, false)

	uid, gid := noACLOtherUID, noACLOtherUID
	sess := h.CreateSession("127.0.0.1:23456", false, "user-b", "")
	sess.User = &models.User{Username: "user-b", UID: &uid, Groups: []models.Group{{GID: &gid}}}
	const treeB uint32 = 2
	h.StoreTree(&TreeConnection{TreeID: treeB, SessionID: sess.SessionID, ShareName: ctxA.ShareName, Permission: models.PermissionReadWrite})
	ctxB = &SMBHandlerContext{Context: context.Background(), TreeID: treeB, SessionID: sess.SessionID, ShareName: ctxA.ShareName}

	metaSvc := h.Registry.GetMetadataService()
	rootUID, rootGID := uint32(0), uint32(0)
	rootAuth := &metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID}}
	d, _, err := metaSvc.CreateDirectory(rootAuth, root, "adir", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755, UID: noACLOwnerUID, GID: noACLOwnerUID,
	})
	if err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	dir, err = metadata.EncodeFileHandle(d)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	if _, _, err := metaSvc.CreateFile(rootAuth, dir, "a.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644, UID: noACLOwnerUID, GID: noACLOwnerUID,
	}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	return h, ctxA, ctxB, dir
}

func noACLFileExists(t *testing.T, h *Handler, dir metadata.FileHandle) bool {
	t.Helper()
	for _, name := range dirTestChildNames(t, h, dir) {
		if name == "a.txt" {
			return true
		}
	}
	return false
}

func closeNoACL(t *testing.T, h *Handler, ctx *SMBHandlerContext, fileID [16]byte) {
	t.Helper()
	resp, err := h.Close(ctx, &CloseRequest{FileID: fileID})
	if err != nil || resp.Status != types.StatusSuccess {
		t.Fatalf("Close: status=%v err=%v", resp.Status, err)
	}
}

func TestCreate_NoACL_OtherUserCannotDeleteOnClose(t *testing.T) {
	h, _, ctxB, dir := setupNoACLDeleteShare(t)

	resp, err := h.Create(ctxB, &CreateRequest{
		FileName:          "adir\\a.txt",
		DesiredAccess:     uint32(types.Delete),
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateOptions:     types.FileNonDirectoryFile | types.FileDeleteOnClose,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.Status == types.StatusSuccess {
		closeNoACL(t, h, ctxB, resp.FileID)
	}
	if resp.Status != types.StatusAccessDenied {
		t.Errorf("B's CREATE(DELETE, DELETE_ON_CLOSE) on A's 0644 file = %v, want STATUS_ACCESS_DENIED", resp.Status)
	}
	if !noACLFileExists(t, h, dir) {
		t.Fatal("B deleted A's file through delete-on-close")
	}
}

func TestSetInfo_NoACL_OtherUserCannotSetDisposition(t *testing.T) {
	h, _, ctxB, dir := setupNoACLDeleteShare(t)

	// MAXIMUM_ALLOWED never fails the open; it must resolve to the rights the
	// mode grants B, which carry no DELETE.
	resp, err := h.Create(ctxB, &CreateRequest{
		FileName:          "adir\\a.txt",
		DesiredAccess:     uint32(types.MaximumAllowed),
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateOptions:     types.FileNonDirectoryFile,
	})
	if err != nil || resp.Status != types.StatusSuccess {
		t.Fatalf("B's CREATE(MAXIMUM_ALLOWED): status=%v err=%v", resp.Status, err)
	}
	si, err := h.SetInfo(ctxB, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileDispositionInformation),
		FileID:        resp.FileID,
		Buffer:        encodeDispositionInfo(true),
	})
	if err != nil {
		t.Fatalf("SetInfo: %v", err)
	}
	closeNoACL(t, h, ctxB, resp.FileID)
	if si.Status != types.StatusAccessDenied {
		t.Errorf("B's SET_INFO FileDispositionInformation on A's 0644 file = %v, want STATUS_ACCESS_DENIED", si.Status)
	}
	if !noACLFileExists(t, h, dir) {
		t.Fatal("B deleted A's file through SET_INFO FileDispositionInformation")
	}
}

func TestCreate_NoACL_OwnerCanDeleteOnClose(t *testing.T) {
	h, ctxA, _, dir := setupNoACLDeleteShare(t)

	resp, err := h.Create(ctxA, &CreateRequest{
		FileName:          "adir\\a.txt",
		DesiredAccess:     uint32(types.Delete),
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateOptions:     types.FileNonDirectoryFile | types.FileDeleteOnClose,
	})
	if err != nil || resp.Status != types.StatusSuccess {
		t.Fatalf("A's CREATE(DELETE, DELETE_ON_CLOSE): status=%v err=%v", resp.Status, err)
	}
	closeNoACL(t, h, ctxA, resp.FileID)
	if noACLFileExists(t, h, dir) {
		t.Fatal("A's own delete-on-close did not remove the file")
	}
}

// A write-granted handle skips the per-op POSIX check, so the open is the only
// gate keeping B from overwriting A's 0644 file.
func TestCreate_NoACL_OtherUserCannotOpenForWrite(t *testing.T) {
	h, _, ctxB, _ := setupNoACLDeleteShare(t)

	resp, err := h.Create(ctxB, &CreateRequest{
		FileName:          "adir\\a.txt",
		DesiredAccess:     uint32(types.FileWriteData),
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateOptions:     types.FileNonDirectoryFile,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.Status == types.StatusSuccess {
		closeNoACL(t, h, ctxB, resp.FileID)
	}
	if resp.Status != types.StatusAccessDenied {
		t.Errorf("B's CREATE(FILE_WRITE_DATA) on A's 0644 file = %v, want STATUS_ACCESS_DENIED", resp.Status)
	}
}

// MAXIMUM_ALLOWED resolves to the rights the mode grants B, which carry no
// DELETE, so delete-on-close must be refused against the resolved grant rather
// than accepted on the MAXIMUM_ALLOWED bit — CLOSE would otherwise unlink A's
// file without re-checking.
func TestCreate_NoACL_OtherUserMaximumAllowedCannotDeleteOnClose(t *testing.T) {
	h, _, ctxB, dir := setupNoACLDeleteShare(t)

	resp, err := h.Create(ctxB, &CreateRequest{
		FileName:          "adir\\a.txt",
		DesiredAccess:     uint32(types.MaximumAllowed),
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateOptions:     types.FileNonDirectoryFile | types.FileDeleteOnClose,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.Status == types.StatusSuccess {
		closeNoACL(t, h, ctxB, resp.FileID)
	}
	if resp.Status != types.StatusAccessDenied {
		t.Errorf("B's CREATE(MAXIMUM_ALLOWED, DELETE_ON_CLOSE) on A's 0644 file = %v, want STATUS_ACCESS_DENIED", resp.Status)
	}
	if !noACLFileExists(t, h, dir) {
		t.Fatal("B deleted A's file through MAXIMUM_ALLOWED delete-on-close")
	}
}
