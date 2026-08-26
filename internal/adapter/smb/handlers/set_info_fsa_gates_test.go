package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Gates that MS-FSA states as MUSTs on the SET_INFO path and that the handler
// previously did not apply. Each case is paired with a control that must take
// the opposite branch, so a test cannot pass by the gate never being reached.

func encodeDispositionExInfo(flags uint32) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, flags)
	return buf
}

func encodeAllocationInfo(size uint64) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, size)
	return buf
}

// TestSetInfo_DispositionEx_IgnoreReadonlyAttribute pins MS-FSCC 2.4.12:
// FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE "allows files with the READ_ONLY
// attribute to be deleted anyway", and without it the refusal is a MUST. Both
// directions are asserted; honouring only bit 0 fails the first case.
func TestSetInfo_DispositionEx_IgnoreReadonlyAttribute(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)

	newReadonly := func(name string) [16]byte {
		t.Helper()
		resp, err := h.Create(ctx, &CreateRequest{
			FileName:          name,
			DesiredAccess:     secRightsFileAll,
			FileAttributes:    types.FileAttributeReadonly,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpenIf,
			CreateOptions:     types.FileNonDirectoryFile,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
		if resp.Status != types.StatusSuccess {
			t.Fatalf("Create(%q) = %v, want STATUS_SUCCESS", name, resp.Status)
		}
		return resp.FileID
	}

	disposition := func(fid [16]byte, flags uint32) types.Status {
		t.Helper()
		resp, err := h.SetInfo(ctx, &SetInfoRequest{
			InfoType:      types.SMB2InfoTypeFile,
			FileInfoClass: uint8(types.FileDispositionInformationEx),
			FileID:        fid,
			Buffer:        encodeDispositionExInfo(flags),
		})
		if err != nil {
			t.Fatalf("SetInfo disposition-ex: %v", err)
		}
		return resp.Status
	}

	// Control: DELETE alone on a read-only file MUST be refused.
	if got := disposition(newReadonly("ro-plain"), types.FileDispositionDelete); got != types.StatusCannotDelete {
		t.Errorf("DELETE on a read-only file = %v, want STATUS_CANNOT_DELETE", got)
	}

	// DELETE|IGNORE_READONLY_ATTRIBUTE must be permitted.
	flags := types.FileDispositionDelete | types.FileDispositionIgnoreReadonlyAttribute
	if got := disposition(newReadonly("ro-ignored"), flags); got != types.StatusSuccess {
		t.Errorf("DELETE|IGNORE_READONLY on a read-only file = %v, want STATUS_SUCCESS", got)
	}
}

// TestSetInfo_DispositionEx_OnCloseRequiresDeleteOnClose pins MS-FSCC 2.4.12:
// FILE_DISPOSITION_ON_CLOSE "set and the file is not opened with
// FILE_DELETE_ON_CLOSE, STATUS_NOT_SUPPORTED MUST be returned".
func TestSetInfo_DispositionEx_OnCloseRequiresDeleteOnClose(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)

	open := func(name string, opts types.CreateOptions) [16]byte {
		t.Helper()
		return dirTestCreate(t, h, ctx, name, types.FileOpenIf, types.FileNonDirectoryFile|opts)
	}

	onClose := types.FileDispositionDelete | types.FileDispositionOnClose
	set := func(fid [16]byte) types.Status {
		t.Helper()
		resp, err := h.SetInfo(ctx, &SetInfoRequest{
			InfoType:      types.SMB2InfoTypeFile,
			FileInfoClass: uint8(types.FileDispositionInformationEx),
			FileID:        fid,
			Buffer:        encodeDispositionExInfo(onClose),
		})
		if err != nil {
			t.Fatalf("SetInfo disposition-ex: %v", err)
		}
		return resp.Status
	}

	if got := set(open("doc-absent", 0)); got != types.StatusNotSupported {
		t.Errorf("ON_CLOSE without FILE_DELETE_ON_CLOSE = %v, want STATUS_NOT_SUPPORTED", got)
	}

	// Control: the same flags on a handle opened delete-on-close are accepted.
	if got := set(open("doc-present", types.FileDeleteOnClose)); got != types.StatusSuccess {
		t.Errorf("ON_CLOSE on a delete-on-close handle = %v, want STATUS_SUCCESS", got)
	}
}

// TestSetInfo_Rename_RefusedWhenDeletePending pins MS-FSA 2.1.5.15.12: "If
// Open.Link.IsDeleted is TRUE, the operation MUST be failed with
// STATUS_ACCESS_DENIED."
func TestSetInfo_Rename_RefusedWhenDeletePending(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)

	rename := func(fid [16]byte, to string) types.Status {
		t.Helper()
		resp, err := h.SetInfo(ctx, &SetInfoRequest{
			InfoType:      types.SMB2InfoTypeFile,
			FileInfoClass: uint8(types.FileRenameInformation),
			FileID:        fid,
			Buffer:        encodeRenameInfo(to),
		})
		if err != nil {
			t.Fatalf("SetInfo rename: %v", err)
		}
		return resp.Status
	}

	// Control: a rename with no disposition set succeeds, so the refusal below
	// cannot be an artifact of the rename path being broken outright.
	if got := rename(dirTestCreate(t, h, ctx, "live", types.FileOpenIf, types.FileNonDirectoryFile), "live-moved"); got != types.StatusSuccess {
		t.Fatalf("rename of a live link = %v, want STATUS_SUCCESS", got)
	}

	doomed := dirTestCreate(t, h, ctx, "doomed", types.FileOpenIf, types.FileNonDirectoryFile)
	dispResp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileDispositionInformation),
		FileID:        doomed,
		Buffer:        encodeDispositionInfo(true),
	})
	if err != nil {
		t.Fatalf("SetInfo disposition: %v", err)
	}
	if dispResp.Status != types.StatusSuccess {
		t.Fatalf("disposition = %v, want STATUS_SUCCESS", dispResp.Status)
	}

	if got := rename(doomed, "doomed-moved"); got != types.StatusAccessDenied {
		t.Errorf("rename of a link marked deleted = %v, want STATUS_ACCESS_DENIED", got)
	}
}

// TestSetInfo_SetAllocation_RequiresWriteData pins MS-FSA 2.1.5.15.1: "If
// Open.GrantedAccess does not contain FILE_WRITE_DATA, the operation MUST be
// failed with STATUS_ACCESS_DENIED."
func TestSetInfo_SetAllocation_RequiresWriteData(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)

	open := func(name string, access uint32) [16]byte {
		t.Helper()
		resp, err := h.Create(ctx, &CreateRequest{
			FileName:          name,
			DesiredAccess:     access,
			FileAttributes:    types.FileAttributeNormal,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpenIf,
			CreateOptions:     types.FileNonDirectoryFile,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
		if resp.Status != types.StatusSuccess {
			t.Fatalf("Create(%q) = %v, want STATUS_SUCCESS", name, resp.Status)
		}
		return resp.FileID
	}

	setAlloc := func(fid [16]byte) types.Status {
		t.Helper()
		resp, err := h.SetInfo(ctx, &SetInfoRequest{
			InfoType:      types.SMB2InfoTypeFile,
			FileInfoClass: uint8(types.FileAllocationInformation),
			FileID:        fid,
			Buffer:        encodeAllocationInfo(4096),
		})
		if err != nil {
			t.Fatalf("SetInfo allocation: %v", err)
		}
		return resp.Status
	}

	readOnly := uint32(types.FileReadData) | uint32(types.FileReadAttributes)
	if got := setAlloc(open("alloc-ro", readOnly)); got != types.StatusAccessDenied {
		t.Errorf("SetAlloc without FILE_WRITE_DATA = %v, want STATUS_ACCESS_DENIED", got)
	}

	// Control: the same call on a writable handle is accepted.
	if got := setAlloc(open("alloc-rw", secRightsFileAll)); got != types.StatusSuccess {
		t.Errorf("SetAlloc on a writable handle = %v, want STATUS_SUCCESS", got)
	}
}

// TestSetInfo_Rename_AllowedOnDeleteOnCloseHandle guards the failure mode the
// rename gate above could plausibly introduce. Open.Link.IsDeleted is the
// disposition set through SET_INFO, not the FILE_DELETE_ON_CLOSE create option:
// that option is held per-handle as InitialDeleteOnClose and promoted to
// DeletePending only during the CLOSE election, matching MS-FSA 2.1.5.5 phase 1.
// Conflating the two would make every delete-on-close handle un-renameable.
func TestSetInfo_Rename_AllowedOnDeleteOnCloseHandle(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)

	resp, err := h.Create(ctx, &CreateRequest{
		FileName:          "doc-renamable",
		DesiredAccess:     secRightsFileAll,
		FileAttributes:    types.FileAttributeNormal,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpenIf,
		CreateOptions:     types.FileNonDirectoryFile | types.FileDeleteOnClose,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("Create = %v, want STATUS_SUCCESS", resp.Status)
	}

	setResp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileRenameInformation),
		FileID:        resp.FileID,
		Buffer:        encodeRenameInfo("doc-renamed"),
	})
	if err != nil {
		t.Fatalf("SetInfo rename: %v", err)
	}
	if setResp.Status != types.StatusSuccess {
		t.Errorf("rename of a delete-on-close handle = %v, want STATUS_SUCCESS", setResp.Status)
	}
}

// TestSetInfo_Rename_RefusedOnSecondHandleWhenDeletePending covers the case the
// single-handle form of this gate missed. Open.Link.IsDeleted is a property of
// the link, so a disposition committed on one handle must block a rename issued
// on any other open of the same file — and DeletePending is not copied onto
// sibling handles until the CLOSE election runs.
func TestSetInfo_Rename_RefusedOnSecondHandleWhenDeletePending(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)

	first := dirTestCreate(t, h, ctx, "shared", types.FileOpenIf, types.FileNonDirectoryFile)
	second := dirTestCreate(t, h, ctx, "shared", types.FileOpen, types.FileNonDirectoryFile)
	if first == second {
		t.Fatal("expected two distinct opens on the same file")
	}

	dispResp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileDispositionInformation),
		FileID:        first,
		Buffer:        encodeDispositionInfo(true),
	})
	if err != nil {
		t.Fatalf("SetInfo disposition on first handle: %v", err)
	}
	if dispResp.Status != types.StatusSuccess {
		t.Fatalf("disposition = %v, want STATUS_SUCCESS", dispResp.Status)
	}

	// The rename arrives on the OTHER handle, which never set a disposition of
	// its own and so still reports DeletePending == false.
	renResp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileRenameInformation),
		FileID:        second,
		Buffer:        encodeRenameInfo("shared-moved"),
	})
	if err != nil {
		t.Fatalf("SetInfo rename on second handle: %v", err)
	}
	if renResp.Status != types.StatusAccessDenied {
		t.Errorf("rename on a sibling handle of a doomed link = %v, want STATUS_ACCESS_DENIED", renResp.Status)
	}
}
