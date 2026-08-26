package handlers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestSetInfo_Rename_UpdatesChangeTimeAndLeavesWriteTime pins MS-FSA 2.1.5.15.12:
// "The object store MUST update Open.File.LastChangeTime." The section never
// touches LastModificationTime, and Move already implements both halves — the
// handler used to snapshot and write back both stamps, reverting the ctime
// update the spec requires.
func TestSetInfo_Rename_UpdatesChangeTimeAndLeavesWriteTime(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)
	metaSvc := h.Registry.GetMetadataService()
	auth := dirTestAuthContext()

	fileID := dirTestCreate(t, h, ctx, "before", types.FileOpenIf, types.FileNonDirectoryFile)
	v, ok := h.files.Load(string(fileID[:]))
	if !ok {
		t.Fatal("open file not found after create")
	}
	handle := v.(*OpenFile).MetadataHandle

	// Backdate both stamps directly rather than through SET_INFO: an explicit
	// SET_INFO would freeze the field, which is the Open.UserSetChangeTime case
	// product note <187> licenses and which restoreFrozenTimestamps still honours.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(auth, handle, &metadata.SetAttrs{Mtime: &old, Ctime: &old}); err != nil {
		t.Fatalf("backdate timestamps: %v", err)
	}

	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileRenameInformation),
		FileID:        fileID,
		Buffer:        encodeRenameInfo("after"),
	})
	if err != nil {
		t.Fatalf("SetInfo rename: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("rename = %v, want STATUS_SUCCESS", resp.Status)
	}

	file, err := metaSvc.GetFile(auth.Context, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !file.Ctime.After(old) {
		t.Errorf("Ctime = %v after rename; want it advanced past %v", file.Ctime.UTC(), old)
	}
	if !file.Mtime.Equal(old) {
		t.Errorf("Mtime = %v after rename; want %v unchanged", file.Mtime.UTC(), old)
	}
}

// TestSetInfo_Rename_KeepsFrozenChangeTime is the control for the above: the
// -1 sentinel freeze is what product note <187> describes ("the file system only
// updates LastChangeTime if no user has explicitly set LastChangeTime"), so
// dropping the blanket save/restore must not disturb it.
func TestSetInfo_Rename_KeepsFrozenChangeTime(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)
	metaSvc := h.Registry.GetMetadataService()
	auth := dirTestAuthContext()

	fileID := dirTestCreate(t, h, ctx, "frozen-before", types.FileOpenIf, types.FileNonDirectoryFile)
	v, ok := h.files.Load(string(fileID[:]))
	if !ok {
		t.Fatal("open file not found after create")
	}
	open := v.(*OpenFile)

	// Explicit ChangeTime through SET_INFO freezes the field.
	pinned := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	basic := encodeBasicInfo(0, 0, 0, types.TimeToFiletime(pinned), 0)
	setResp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileBasicInformation),
		FileID:        fileID,
		Buffer:        basic,
	})
	if err != nil || setResp.Status != types.StatusSuccess {
		t.Fatalf("SetInfo basic: err=%v status=%v", err, setResp)
	}
	if !open.CtimeFrozen {
		t.Fatal("precondition: an explicit ChangeTime set must freeze the field")
	}

	renResp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileRenameInformation),
		FileID:        fileID,
		Buffer:        encodeRenameInfo("frozen-after"),
	})
	if err != nil {
		t.Fatalf("SetInfo rename: %v", err)
	}
	if renResp.Status != types.StatusSuccess {
		t.Fatalf("rename = %v, want STATUS_SUCCESS", renResp.Status)
	}

	file, err := metaSvc.GetFile(auth.Context, open.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !file.Ctime.Equal(pinned) {
		t.Errorf("frozen Ctime = %v after rename; want %v preserved", file.Ctime.UTC(), pinned)
	}
}
