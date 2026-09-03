// Where the directory-emptiness refusal belongs.
//
// MS-FSA splits the two halves of a delete-on-close rmdir:
//
//   - 2.1.5.15.3 step 3.2.1 (Set FileDispositionInformation) refuses to mark a
//     directory that still holds entries, with STATUS_DIRECTORY_NOT_EMPTY.
//   - 2.1.5.5 phase 1 step 2.1.1 (Close) marks the link deleted only when the
//     directory's entry list is empty. A non-empty directory is left in place
//     and the close still completes with STATUS_SUCCESS — the close has no
//     STATUS_DIRECTORY_NOT_EMPTY outcome at all.
//
// DittoFS had these the wrong way round: the disposition set accepted any
// directory and the close surfaced the store's ErrNotEmpty as a failed CLOSE.
// That failed the composite rmdir smbtorture builds out of
// CREATE(FILE_DELETE_ON_CLOSE) + CLOSE whenever the directory still held an
// entry whose own removal was deferred to a handle that stayed open.
package handlers

import (
	"context"
	"encoding/binary"
	"testing"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// secRightsFileAll is SEC_RIGHTS_FILE_ALL: every file-specific right plus the
// standard rights, DELETE included, as the notify torture opens request.
const secRightsFileAll = uint32(0x001F01FF)

// dirTestAuthContext builds an AuthContext for the identity
// setupStreamsDisabledShare gives its session.
func dirTestAuthContext() *metadata.AuthContext {
	uid, gid := uint32(1000), uint32(1000)
	return &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
}

// encodeRenameInfo builds a FILE_RENAME_INFORMATION buffer [MS-FSCC] 2.4.42.2
// naming newName, share-relative.
func encodeRenameInfo(newName string) []byte {
	name := utf16.Encode([]rune(newName))
	buf := make([]byte, 20+len(name)*2)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(name)*2))
	for i, c := range name {
		binary.LittleEndian.PutUint16(buf[20+i*2:], c)
	}
	return buf
}

// encodeDispositionInfo builds a FILE_DISPOSITION_INFORMATION buffer
// [MS-FSCC] 2.4.11.
func encodeDispositionInfo(deletePending bool) []byte {
	if deletePending {
		return []byte{1}
	}
	return []byte{0}
}

func dirTestCreate(t *testing.T, h *Handler, ctx *SMBHandlerContext, fname string,
	disp types.CreateDisposition, opts types.CreateOptions,
) [16]byte {
	t.Helper()
	resp, err := h.Create(ctx, &CreateRequest{
		FileName:          fname,
		DesiredAccess:     secRightsFileAll,
		FileAttributes:    types.FileAttributeNormal,
		ShareAccess:       0x07, // R|W|D
		CreateDisposition: disp,
		CreateOptions:     opts,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", fname, err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("Create(%q) = %v, want STATUS_SUCCESS", fname, resp.Status)
	}
	return resp.FileID
}

func dirTestRename(t *testing.T, h *Handler, ctx *SMBHandlerContext, fileID [16]byte, newName string) {
	t.Helper()
	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileRenameInformation),
		FileID:        fileID,
		Buffer:        encodeRenameInfo(newName),
	})
	if err != nil {
		t.Fatalf("rename to %q: %v", newName, err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("rename to %q = %v, want STATUS_SUCCESS", newName, resp.Status)
	}
}

// dirTestRmdir mirrors smb2_util_rmdir: a CREATE carrying
// FILE_DELETE_ON_CLOSE followed by a CLOSE. It returns the CLOSE status, or
// the CREATE status when the open itself is refused — the same status
// smb2_util_rmdir hands its caller.
func dirTestRmdir(t *testing.T, h *Handler, ctx *SMBHandlerContext, fname string) types.Status {
	t.Helper()
	resp, err := h.Create(ctx, &CreateRequest{
		FileName:          fname,
		DesiredAccess:     uint32(types.Delete),
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateOptions:     types.FileDirectoryFile | types.FileDeleteOnClose,
	})
	if err != nil {
		t.Fatalf("rmdir open(%q): %v", fname, err)
	}
	if resp.Status != types.StatusSuccess {
		return resp.Status
	}
	closeResp, err := h.Close(ctx, &CloseRequest{FileID: resp.FileID})
	if err != nil {
		t.Fatalf("rmdir close(%q): %v", fname, err)
	}
	return closeResp.Status
}

// dirTestChildNames returns the entry names under parent.
func dirTestChildNames(t *testing.T, h *Handler, parent metadata.FileHandle) []string {
	t.Helper()
	page, err := h.Registry.GetMetadataService().ReadDirectory(dirTestAuthContext(), parent, 0, 1<<20)
	if err != nil {
		t.Fatalf("ReadDirectory: %v", err)
	}
	names := make([]string, 0, len(page.Entries))
	for _, e := range page.Entries {
		names = append(names, e.Name)
	}
	return names
}

// dirTestLookup resolves name under parent to a file handle.
func dirTestLookup(t *testing.T, h *Handler, parent metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()
	file, _, err := h.Registry.GetMetadataService().LookupCaseInsensitive(dirTestAuthContext(), parent, name)
	if err != nil {
		t.Fatalf("lookup %q: %v", name, err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle(%q): %v", name, err)
	}
	return handle
}

// TestClose_DeleteOnClose_NonEmptyDirectorySucceeds replays the rmdir tail of
// source4/torture/smb2/notify.c::torture_smb2_notify_recursive (the same
// sequence torture_smb2_notify_mask_change repeats), which is what
// smb2.notify.rec and smb2.notify.mask-change trip over.
//
// The sequence renames a subdirectory and a file, leaving every handle open —
// the torture overwrites io1.smb2.out.file.handle without closing it — and
// then rmdirs the subdirectory and its parent, requiring STATUS_OK from both.
//
// The subdirectory's own rmdir cannot unlink it: a handle on the same link is
// still open, so per MS-FSA 2.1.5.5 phase 3 step 8.1 the link stays. The
// parent's rmdir therefore meets a non-empty directory, and per phase 1 step
// 2.1.1 it must decline the removal and still return STATUS_SUCCESS.
func TestClose_DeleteOnClose_NonEmptyDirectorySucceeds(t *testing.T) {
	h, ctx, root := setupStreamsDisabledShare(t, false)

	// subdir-name, closed straight away.
	fileID := dirTestCreate(t, h, ctx, "subdir-name", types.FileOpenIf, types.FileDirectoryFile)
	cr, err := h.Close(ctx, &CloseRequest{FileID: fileID})
	if err != nil {
		t.Fatalf("close subdir-name: %v", err)
	}
	if cr.GetStatus() != types.StatusSuccess {
		t.Fatalf("close subdir-name status = %v, want STATUS_SUCCESS", cr.GetStatus())
	}

	// subname1 (a directory) renamed within subdir-name; handle left open.
	subdirFileID := dirTestCreate(t, h, ctx, "subdir-name\\subname1", types.FileOpenIf, types.FileDirectoryFile)
	dirTestRename(t, h, ctx, subdirFileID, "subdir-name\\subname1-r")

	// subname2 (a file) renamed out of subdir-name; handle left open.
	movedFileID := dirTestCreate(t, h, ctx, "subdir-name\\subname2", types.FileOpenIf, types.FileNonDirectoryFile)
	dirTestRename(t, h, ctx, movedFileID, "subname2-r")

	// Re-open at the destination and rename again; handle left open.
	reopenedFileID := dirTestCreate(t, h, ctx, "subname2-r", types.FileOpen, types.FileNonDirectoryFile)
	dirTestRename(t, h, ctx, reopenedFileID, "subname3-r")

	subdirHandle := dirTestLookup(t, h, root, "subdir-name")

	// The rename out of subdir-name did land: the only entry left is the
	// renamed subdirectory, not the moved file.
	if got := dirTestChildNames(t, h, subdirHandle); len(got) != 1 || got[0] != "subname1-r" {
		t.Fatalf("subdir-name holds %v, want exactly [subname1-r] — the cross-directory rename left something behind", got)
	}

	if status := dirTestRmdir(t, h, ctx, "subdir-name\\subname1-r"); status != types.StatusSuccess {
		t.Fatalf("rmdir subname1-r = %v, want STATUS_SUCCESS", status)
	}
	// Its removal is deferred: a handle on that same link is still open.
	if got := dirTestChildNames(t, h, subdirHandle); len(got) != 1 {
		t.Fatalf("subdir-name holds %v; the deferral this test depends on did not happen", got)
	}

	if status := dirTestRmdir(t, h, ctx, "subdir-name"); status != types.StatusSuccess {
		t.Errorf("rmdir subdir-name = %v, want STATUS_SUCCESS: a delete-on-close CLOSE of a "+
			"non-empty directory declines the removal, it does not fail (MS-FSA 2.1.5.5 phase 1); "+
			"subdir-name holds %v", status, dirTestChildNames(t, h, subdirHandle))
	}

	// Declining is not deleting: subdir-name is still there.
	if got := dirTestChildNames(t, h, root); len(got) != 2 {
		t.Errorf("share root holds %v, want subdir-name and subname3-r", got)
	}
}

// TestSetInfo_DeleteDisposition_NonEmptyDirectoryRefused pins the other half:
// STATUS_DIRECTORY_NOT_EMPTY belongs to the disposition set (MS-FSA 2.1.5.15.3
// step 3.2.1), which is the path the Linux SMB client's rmdir takes. Without
// it, moving the refusal off CLOSE would make a failed rmdir look like it
// worked.
func TestSetInfo_DeleteDisposition_NonEmptyDirectoryRefused(t *testing.T) {
	h, ctx, root := setupStreamsDisabledShare(t, false)

	parentID := dirTestCreate(t, h, ctx, "parent", types.FileOpenIf, types.FileDirectoryFile)
	childID := dirTestCreate(t, h, ctx, "parent\\child", types.FileOpenIf, types.FileNonDirectoryFile)
	childClose, err := h.Close(ctx, &CloseRequest{FileID: childID})
	if err != nil {
		t.Fatalf("close child: %v", err)
	}
	if childClose.GetStatus() != types.StatusSuccess {
		t.Fatalf("close child status = %v, want STATUS_SUCCESS", childClose.GetStatus())
	}

	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileDispositionInformation),
		FileID:        parentID,
		Buffer:        encodeDispositionInfo(true),
	})
	if err != nil {
		t.Fatalf("SetInfo disposition: %v", err)
	}
	if resp.Status != types.StatusDirectoryNotEmpty {
		t.Fatalf("disposition on a non-empty directory = %v, want STATUS_DIRECTORY_NOT_EMPTY", resp.Status)
	}

	// The refusal left no delete pending behind: the close is clean and the
	// directory survives.
	closeResp, err := h.Close(ctx, &CloseRequest{FileID: parentID})
	if err != nil {
		t.Fatalf("close parent: %v", err)
	}
	if closeResp.Status != types.StatusSuccess {
		t.Errorf("close after a refused disposition = %v, want STATUS_SUCCESS", closeResp.Status)
	}
	if got := dirTestChildNames(t, h, root); len(got) != 1 || got[0] != "parent" {
		t.Errorf("share root holds %v, want [parent]", got)
	}
}

// TestSetInfo_DeleteDisposition_EmptyDirectoryStillDeletes guards the fix
// against over-refusing: an empty directory's disposition is accepted and the
// close removes it.
func TestSetInfo_DeleteDisposition_EmptyDirectoryStillDeletes(t *testing.T) {
	h, ctx, root := setupStreamsDisabledShare(t, false)

	dirID := dirTestCreate(t, h, ctx, "empty-dir", types.FileOpenIf, types.FileDirectoryFile)

	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileDispositionInformation),
		FileID:        dirID,
		Buffer:        encodeDispositionInfo(true),
	})
	if err != nil {
		t.Fatalf("SetInfo disposition: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("disposition on an empty directory = %v, want STATUS_SUCCESS", resp.Status)
	}

	closeResp, err := h.Close(ctx, &CloseRequest{FileID: dirID})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closeResp.Status != types.StatusSuccess {
		t.Fatalf("close = %v, want STATUS_SUCCESS", closeResp.Status)
	}
	if got := dirTestChildNames(t, h, root); len(got) != 0 {
		t.Errorf("share root holds %v, want the directory to be gone", got)
	}
}
