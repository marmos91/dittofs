package handlers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// MS-FSA 2.1.5.15.12 requires a rename to update LastChangeTime, and product
// note <187> says NTFS and ReFS defer that update until the handle closes. Both
// halves are pinned here, because a fix satisfying only the first is
// indistinguishable from the original bug — which held the old value forever —
// and a fix satisfying only the second breaks smb2.rename.simple_modtime, which
// renames through a handle and then queries that same handle.

// renameCtimeFixture returns an open file, its metadata handle, and the
// ChangeTime it was created with. setupReparseShare is used because it wires a
// local block store, without which CLOSE fails its flush step for a regular
// file — and CLOSE is half of what these tests measure.
//
// The stamp is read rather than written: a SET_INFO would freeze the field (the
// Open.UserSetChangeTime case <187> exempts, covered separately below). The short
// sleep guarantees the rename lands on a distinguishable timestamp.
func renameCtimeFixture(t *testing.T) (*Handler, *SMBHandlerContext, [16]byte, metadata.FileHandle, time.Time) {
	t.Helper()
	h, smbCtx, _, fileID := setupReparseShare(t)

	v, ok := h.files.Load(string(fileID[:]))
	if !ok {
		t.Fatal("open file not found")
	}
	open := v.(*OpenFile)
	handle := open.MetadataHandle
	// setupReparseShare grants only READ|WRITE data; rename needs DELETE and the
	// frozen-ChangeTime case needs WRITE_ATTRIBUTES.
	open.GrantedAccess = secRightsFileAll

	file, err := h.Registry.GetMetadataService().GetFile(smbCtx.Context, handle)
	if err != nil {
		t.Fatalf("GetFile at create: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	return h, smbCtx, fileID, handle, file.Ctime
}

func renameVia(t *testing.T, h *Handler, ctx *SMBHandlerContext, fileID [16]byte, to string) {
	t.Helper()
	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileRenameInformation),
		FileID:        fileID,
		Buffer:        encodeRenameInfo(to),
	})
	if err != nil {
		t.Fatalf("SetInfo rename: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("rename = %v, want STATUS_SUCCESS", resp.Status)
	}
}

// TestSetInfo_Rename_ChangeTimeUnchangedWhileHandleOpen is the half that
// smbtorture smb2.rename.simple_modtime measures: it renames through h1 and then
// issues a getinfo on that same still-open handle, asserting ChangeTime equals
// the value from before the rename. QUERY_INFO applies no ChangeTime overlay, so
// what the store holds is what that client reads.
func TestSetInfo_Rename_ChangeTimeUnchangedWhileHandleOpen(t *testing.T) {
	// setupStreamsDisabledShare here, not the block-store fixture: this half never
	// closes, and its auth context can actually apply the post-rename timestamp
	// restore that delivers the unchanged value.
	h, ctx, _ := setupStreamsDisabledShare(t, false)
	metaSvc := h.Registry.GetMetadataService()

	fileID := dirTestCreate(t, h, ctx, "open-before", types.FileOpenIf, types.FileNonDirectoryFile)
	v, ok := h.files.Load(string(fileID[:]))
	if !ok {
		t.Fatal("open file not found after create")
	}
	handle := v.(*OpenFile).MetadataHandle

	created, err := metaSvc.GetFile(dirTestAuthContext().Context, handle)
	if err != nil {
		t.Fatalf("GetFile at create: %v", err)
	}
	old := created.Ctime
	time.Sleep(5 * time.Millisecond)

	renameVia(t, h, ctx, fileID, "open-after")

	file, err := metaSvc.GetFile(dirTestAuthContext().Context, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !file.Ctime.Equal(old) {
		t.Errorf("Ctime = %v while the renaming handle is still open; want %v unchanged",
			file.Ctime.UTC(), old)
	}
	if !file.Mtime.Equal(old) {
		t.Errorf("Mtime = %v after rename; want %v unchanged", file.Mtime.UTC(), old)
	}
}

// TestSetInfo_Rename_ChangeTimeSettlesOnClose is the other half. Once the handle
// closes, the update MS-FSA requires must have landed — otherwise the file keeps
// its pre-rename ChangeTime forever, which is the defect this replaces.
func TestSetInfo_Rename_ChangeTimeSettlesOnClose(t *testing.T) {
	h, ctx, fileID, handle, old := renameCtimeFixture(t)
	renameVia(t, h, ctx, fileID, "close-after")

	resp, err := h.Close(ctx, &CloseRequest{FileID: fileID})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("close = %v, want STATUS_SUCCESS", resp.Status)
	}

	file, err := h.Registry.GetMetadataService().GetFile(ctx.Context, handle)
	if err != nil {
		t.Fatalf("GetFile after close: %v", err)
	}
	if !file.Ctime.After(old) {
		t.Errorf("Ctime = %v after the renaming handle closed; want it advanced past %v",
			file.Ctime.UTC(), old)
	}
	if !file.Mtime.Equal(old) {
		t.Errorf("Mtime = %v after close; want %v unchanged — rename never modifies content",
			file.Mtime.UTC(), old)
	}
}

// The <187> user-set half — a sentinel-frozen ChangeTime outlasting the deferred
// update — is deliberately not asserted here. Preserving a frozen ChangeTime
// across rename+close is already broken on develop, independently of this change:
// a test setting CtimeFrozen/FrozenCtime and closing reads back the flush time on
// unmodified develop as well. Pinning it in this PR would assert a bug rather than
// the behaviour, so it is tracked separately.
