// Handler-level coverage for the FILE_WRITE_ATTRIBUTES → timestamp-write
// bridge. SMB authorizes a SET_INFO timestamp write by the access right on the
// open handle (MS-FSA 2.1.5.15.2, "FileBasicInformation"), while the metadata
// layer's POSIX gate requires ownership. Without the bridge a non-owner holding
// exactly the right the protocol names is refused.
package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// storeForeignOwnedOpen creates a file owned by UID 2002 with mode 0o600 — so
// the fixture's UID-1000 caller is neither owner nor POSIX-writer — and
// registers a hand-built OpenFile for it carrying grantedAccess.
func storeForeignOwnedOpen(
	t *testing.T,
	h *Handler,
	ctx *SMBHandlerContext,
	rootHandle metadata.FileHandle,
	name string,
	fileID byte,
	grantedAccess uint32,
) (*OpenFile, metadata.FileHandle) {
	t.Helper()

	rootUID, rootGID := uint32(0), uint32(0)
	rootCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID},
	}
	metaSvc := h.Registry.GetMetadataService()
	file, _, err := metaSvc.CreateFile(rootCtx, rootHandle, name, &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o600,
		UID:  2002,
		GID:  2002,
	})
	if err != nil {
		t.Fatalf("CreateFile(%q): %v", name, err)
	}
	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	open := (&OpenFile{
		FileID:         [16]byte{fileID},
		MetadataHandle: fileHandle,
		ShareName:      ctx.ShareName,
		SessionID:      ctx.SessionID,
		TreeID:         ctx.TreeID,
		DesiredAccess:  grantedAccess,
		GrantedAccess:  grantedAccess,
	}).WithName(OpenName{Path: name, FileName: name, ParentHandle: rootHandle})
	h.StoreOpenFile(open)
	return open, fileHandle
}

// TestSetInfo_BasicInfo_WriteAttributesAuthorizesNonOwnerTimestamp asserts a
// non-owner whose handle carries FILE_WRITE_ATTRIBUTES can set a timestamp
// through SET_INFO, and that the stored value actually changed.
func TestSetInfo_BasicInfo_WriteAttributesAuthorizesNonOwnerTimestamp(t *testing.T) {
	h, ctx, rootHandle := setupStreamsDisabledShare(t, false)

	open, fileHandle := storeForeignOwnedOpen(t, h, ctx, rootHandle, "foreign.txt", 0x01,
		uint32(types.FileWriteAttributes)|uint32(types.FileReadAttributes))

	want := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileBasicInformation),
		FileID:        open.FileID,
		Buffer:        encodeBasicInfo(0, 0, types.TimeToFiletime(want), 0, 0),
	})
	if err != nil {
		t.Fatalf("SetInfo: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("SET_INFO BasicInformation on a non-owned file with FILE_WRITE_ATTRIBUTES = %v, want STATUS_SUCCESS", resp.Status)
	}

	file, err := h.Registry.GetMetadataService().GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !file.Mtime.Equal(want) {
		t.Errorf("stored Mtime = %v, want %v — the timestamp write was accepted but did not land",
			file.Mtime.UTC(), want)
	}
}

// TestSetInfo_BasicInfo_WithoutWriteAttributesStillDenied is the control: the
// bridge is keyed on the access right, so a handle lacking
// FILE_WRITE_ATTRIBUTES is still refused at the SET_INFO gate. Without this the
// first test could pass on a build that authorized every SET_INFO.
func TestSetInfo_BasicInfo_WithoutWriteAttributesStillDenied(t *testing.T) {
	h, ctx, rootHandle := setupStreamsDisabledShare(t, false)

	open, fileHandle := storeForeignOwnedOpen(t, h, ctx, rootHandle, "noattr.txt", 0x02,
		uint32(types.FileReadAttributes)|uint32(types.FileReadData))

	before, err := h.Registry.GetMetadataService().GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	wantUnchanged := before.Mtime

	want := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileBasicInformation),
		FileID:        open.FileID,
		Buffer:        encodeBasicInfo(0, 0, types.TimeToFiletime(want), 0, 0),
	})
	if err != nil {
		t.Fatalf("SetInfo: %v", err)
	}
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("SET_INFO BasicInformation without FILE_WRITE_ATTRIBUTES = %v, want STATUS_ACCESS_DENIED", resp.Status)
	}

	after, err := h.Registry.GetMetadataService().GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !after.Mtime.Equal(wantUnchanged) {
		t.Errorf("Mtime moved to %v on a denied SET_INFO; want %v", after.Mtime.UTC(), wantUnchanged.UTC())
	}
}
