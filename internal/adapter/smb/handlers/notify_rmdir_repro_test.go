package handlers

import (
	"context"
	"encoding/binary"
	"testing"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

const secRightsFileAll = uint32(0x001F01FF)

func encodeRenameInfo(newName string) []byte {
	u := utf16.Encode([]rune(newName))
	buf := make([]byte, 20+len(u)*2)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(u)*2))
	for i, c := range u {
		binary.LittleEndian.PutUint16(buf[20+i*2:], c)
	}
	return buf
}

func reproCreate(t *testing.T, h *Handler, ctx *SMBHandlerContext, fname string, disp types.CreateDisposition, opts types.CreateOptions) [16]byte {
	t.Helper()
	resp, err := h.Create(ctx, &CreateRequest{
		FileName:          fname,
		DesiredAccess:     secRightsFileAll,
		FileAttributes:    types.FileAttributeNormal,
		ShareAccess:       0x07,
		CreateDisposition: disp,
		CreateOptions:     opts,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", fname, err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("Create(%q) = 0x%08x", fname, resp.Status)
	}
	return resp.FileID
}

func reproRename(t *testing.T, h *Handler, ctx *SMBHandlerContext, fid [16]byte, newName string) {
	t.Helper()
	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileRenameInformation),
		FileID:        fid,
		Buffer:        encodeRenameInfo(newName),
	})
	if err != nil {
		t.Fatalf("rename -> %q: %v", newName, err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("rename -> %q = 0x%08x", newName, resp.Status)
	}
}

// reproRmdir mirrors smb2_util_rmdir: open the directory with
// FILE_DELETE_ON_CLOSE, then close it.
func reproRmdir(t *testing.T, h *Handler, ctx *SMBHandlerContext, fname string) types.Status {
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
	cresp, err := h.Close(ctx, &CloseRequest{FileID: resp.FileID})
	if err != nil {
		t.Fatalf("rmdir close(%q): %v", fname, err)
	}
	return cresp.Status
}

func listChildren(t *testing.T, h *Handler, parent metadata.FileHandle) []string {
	t.Helper()
	uid, gid := uint32(1000), uint32(1000)
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	page, err := h.Registry.GetMetadataService().ReadDirectory(authCtx, parent, 0, 1<<20)
	if err != nil {
		t.Fatalf("ReadDirectory: %v", err)
	}
	var names []string
	for _, e := range page.Entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		names = append(names, e.Name)
	}
	return names
}

// TestNotifyRec_RmdirSequence replays source4/torture/smb2/notify.c:580-633
// (smb2.notify.rec) through the CREATE / SET_INFO / CLOSE handlers.
func TestNotifyRec_RmdirSequence(t *testing.T) {
	h, ctx, root := setupStreamsDisabledShare(t, false)

	// subdir-name, closed straight away.
	fid := reproCreate(t, h, ctx, "subdir-name", types.FileOpenIf, types.FileDirectoryFile)
	if _, err := h.Close(ctx, &CloseRequest{FileID: fid}); err != nil {
		t.Fatalf("close subdir-name: %v", err)
	}

	// subname1 (dir) -> subname1-r. Handle deliberately left open: the
	// torture test overwrites io1.smb2.out.file.handle without closing.
	h1 := reproCreate(t, h, ctx, "subdir-name\\subname1", types.FileOpenIf, types.FileDirectoryFile)
	reproRename(t, h, ctx, h1, "subdir-name\\subname1-r")

	// subname2 (file) -> subname2-r, out of subdir-name. Also left open.
	h2 := reproCreate(t, h, ctx, "subdir-name\\subname2", types.FileOpenIf, types.FileNonDirectoryFile)
	reproRename(t, h, ctx, h2, "subname2-r")

	// Re-open the destination and rename again. Also left open.
	h3 := reproCreate(t, h, ctx, "subname2-r", types.FileOpen, types.FileNonDirectoryFile)
	reproRename(t, h, ctx, h3, "subname3-r")

	t.Logf("root children before rmdir: %v", listChildren(t, h, root))

	subdirFile, _, err := h.Registry.GetMetadataService().LookupCaseInsensitive(
		&metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{
			UID: func() *uint32 { u := uint32(1000); return &u }(),
			GID: func() *uint32 { g := uint32(1000); return &g }(),
		}}, root, "subdir-name")
	if err != nil {
		t.Fatalf("lookup subdir-name: %v", err)
	}
	subdirHandle, err := metadata.EncodeFileHandle(subdirFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	t.Logf("subdir-name children before rmdir: %v", listChildren(t, h, subdirHandle))

	st := reproRmdir(t, h, ctx, "subdir-name\\subname1-r")
	t.Logf("rmdir subname1-r -> 0x%08x", st)
	t.Logf("subdir-name children after rmdir subname1-r: %v", listChildren(t, h, subdirHandle))

	st = reproRmdir(t, h, ctx, "subdir-name")
	t.Logf("rmdir subdir-name -> 0x%08x", st)
	if st != types.StatusSuccess {
		t.Errorf("rmdir subdir-name = 0x%08x, want STATUS_SUCCESS; leftover children = %v",
			st, listChildren(t, h, subdirHandle))
	}
}

// Variant: close the subname1-r handle before the rmdir.
func TestNotifyRec_RmdirSequence_HandleClosed(t *testing.T) {
	h, ctx, root := setupStreamsDisabledShare(t, false)

	fid := reproCreate(t, h, ctx, "subdir-name", types.FileOpenIf, types.FileDirectoryFile)
	if _, err := h.Close(ctx, &CloseRequest{FileID: fid}); err != nil {
		t.Fatalf("close subdir-name: %v", err)
	}
	h1 := reproCreate(t, h, ctx, "subdir-name\\subname1", types.FileOpenIf, types.FileDirectoryFile)
	reproRename(t, h, ctx, h1, "subdir-name\\subname1-r")
	if _, err := h.Close(ctx, &CloseRequest{FileID: h1}); err != nil {
		t.Fatalf("close subname1-r: %v", err)
	}
	h2 := reproCreate(t, h, ctx, "subdir-name\\subname2", types.FileOpenIf, types.FileNonDirectoryFile)
	reproRename(t, h, ctx, h2, "subname2-r")
	h3 := reproCreate(t, h, ctx, "subname2-r", types.FileOpen, types.FileNonDirectoryFile)
	reproRename(t, h, ctx, h3, "subname3-r")
	_ = root

	st := reproRmdir(t, h, ctx, "subdir-name\\subname1-r")
	t.Logf("rmdir subname1-r -> 0x%08x", st)
	st = reproRmdir(t, h, ctx, "subdir-name")
	t.Logf("rmdir subdir-name -> 0x%08x", st)
	if st != types.StatusSuccess {
		t.Errorf("rmdir subdir-name = 0x%08x with the stale handle closed", st)
	}
}
