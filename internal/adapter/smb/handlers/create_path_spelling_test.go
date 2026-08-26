package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Share-mode and delete-pending conflict scans relate a base file to its
// alternate data streams. That relation cannot come from the metadata handle —
// a stream is a separate metadata entry from its base — so it has to be derived
// from the name. The name a handle carries must therefore identify the object,
// not echo the spelling the client happened to use: SMB CREATE accepts any
// syntactically legal spelling of a path, and two spellings of one path must
// reach the same conflict decision.
//
// These tests pin both halves: that CREATE records a spelling-independent
// identity for the handle, and that both conflict scans key off it.

const (
	testAccessRead   = uint32(0x00000001) // FILE_READ_DATA
	testAccessDelete = uint32(0x00010000) // DELETE
	testShareRead    = uint32(0x01)
	testShareWrite   = uint32(0x02)
	testShareDelete  = uint32(0x04)
)

// spellingCreateRequest builds a CREATE for fname with explicit access and
// sharing masks, so a test can set up a share-mode conflict.
func spellingCreateRequest(
	fname string,
	access, share uint32,
	disp types.CreateDisposition,
	opts types.CreateOptions,
) *CreateRequest {
	return &CreateRequest{
		FileName:          fname,
		DesiredAccess:     access,
		FileAttributes:    types.FileAttributeNormal,
		ShareAccess:       share,
		CreateDisposition: disp,
		CreateOptions:     opts,
	}
}

// mustCreate drives a CREATE through the real handler and fails on any status
// other than the one expected.
func mustCreate(t *testing.T, h *Handler, ctx *SMBHandlerContext, req *CreateRequest) *CreateResponse {
	t.Helper()
	resp, err := h.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create(%q): %v", req.FileName, err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("Create(%q): status = 0x%08x, want STATUS_SUCCESS", req.FileName, uint32(resp.Status))
	}
	return resp
}

// TestCreate_NameIdentityIsSpellingIndependent drives two legal spellings of one
// path through the real CREATE handler and compares the name each open records.
// Every component of both spellings exists, and both resolve to the same object.
//
// The stored Path reproduces whatever the client sent; the (ParentHandle,
// FileName) pair is resolved state and agrees across spellings. Any conflict
// decision that reads Path therefore inherits the client's spelling, while one
// that reads the pair does not.
func TestCreate_NameIdentityIsSpellingIndependent(t *testing.T) {
	h, smbCtx, _ := setupStreamsDisabledShare(t, false)

	mustCreate(t, h, smbCtx, spellingCreateRequest(
		"d", testAccessRead, 0x07, types.FileCreate, types.FileDirectoryFile))
	mustCreate(t, h, smbCtx, spellingCreateRequest(
		"d\\sub", testAccessRead, 0x07, types.FileCreate, types.FileDirectoryFile))
	mustCreate(t, h, smbCtx, spellingCreateRequest(
		"d\\f", testAccessRead, 0x07, types.FileCreate, 0))

	// Two spellings of "d/f": the direct one, and one routing through an
	// existing subdirectory and back out.
	direct := mustCreate(t, h, smbCtx, spellingCreateRequest(
		"d\\f", testAccessRead, 0x07, types.FileOpen, 0))
	viaDotDot := mustCreate(t, h, smbCtx, spellingCreateRequest(
		"d\\sub\\..\\f", testAccessRead, 0x07, types.FileOpen, 0))

	a, ok := h.GetOpenFile(direct.FileID)
	if !ok {
		t.Fatal("open handle for direct spelling not found")
	}
	b, ok := h.GetOpenFile(viaDotDot.FileID)
	if !ok {
		t.Fatal("open handle for ..-spelling not found")
	}

	// Both spellings must have landed on the same object, or the rest of the
	// comparison means nothing.
	if !bytes.Equal(a.MetadataHandle, b.MetadataHandle) {
		t.Fatalf("spellings resolved to different objects: %x vs %x",
			a.MetadataHandle, b.MetadataHandle)
	}

	nameA, nameB := a.Name(), b.Name()

	// Resolved identity agrees across spellings.
	if !bytes.Equal(nameA.ParentHandle, nameB.ParentHandle) {
		t.Errorf("ParentHandle differs across spellings: %x vs %x",
			nameA.ParentHandle, nameB.ParentHandle)
	}
	if nameA.FileName != nameB.FileName {
		t.Errorf("FileName differs across spellings: %q vs %q", nameA.FileName, nameB.FileName)
	}

	// The recorded Path does not: it is the client's own spelling. This is
	// deliberate — diagnostics echo what the client asked for — and is exactly
	// why conflict decisions must not be derived from it.
	if nameA.Path == nameB.Path {
		t.Errorf("Path unexpectedly equal across spellings (%q); "+
			"this test's premise no longer holds", nameA.Path)
	}
}

// TestCheckShareModeConflict_SpellingIndependent holds a stream open that
// refuses delete sharing, then opens the base file for DELETE under two
// spellings. Per MS-FSA 2.1.5.1.2.1 ("Algorithm to Check Access to an Existing File") the base-vs-stream pair is delete-share
// checked, so both opens must be refused with STATUS_SHARING_VIOLATION.
//
// The two spellings differ only in text and name the same object, so a
// difference in outcome is a conflict the server failed to detect.
func TestCheckShareModeConflict_SpellingIndependent(t *testing.T) {
	for _, spelling := range []string{"d\\f", "d\\sub\\..\\f", "d\\\\f", "d\\.\\f"} {
		t.Run(spelling, func(t *testing.T) {
			h, smbCtx, _ := setupStreamsDisabledShare(t, false)

			mustCreate(t, h, smbCtx, spellingCreateRequest(
				"d", testAccessRead, 0x07, types.FileCreate, types.FileDirectoryFile))
			mustCreate(t, h, smbCtx, spellingCreateRequest(
				"d\\sub", testAccessRead, 0x07, types.FileCreate, types.FileDirectoryFile))
			mustCreate(t, h, smbCtx, spellingCreateRequest(
				"d\\f", testAccessRead, 0x07, types.FileCreate, 0))

			// Stream handle holds DELETE and denies delete sharing.
			mustCreate(t, h, smbCtx, spellingCreateRequest(
				"d\\f:s1", testAccessRead|testAccessDelete,
				testShareRead|testShareWrite, types.FileOpenIf, 0))

			// Opening the base for DELETE conflicts: the stream's ShareAccess
			// omits FILE_SHARE_DELETE.
			resp, err := h.Create(smbCtx, spellingCreateRequest(
				spelling, testAccessDelete,
				testShareRead|testShareWrite|testShareDelete, types.FileOpen, 0))
			if err != nil {
				t.Fatalf("Create(%q): %v", spelling, err)
			}
			if resp.Status != types.StatusSharingViolation {
				t.Errorf("Create(%q): status = 0x%08x, want STATUS_SHARING_VIOLATION (0x%08x)",
					spelling, uint32(resp.Status), uint32(types.StatusSharingViolation))
			}
		})
	}
}

// TestIsFileOrBaseDeletePending_SpellingIndependent covers the delete-pending
// arm of the same relation. A stream handle carrying BaseFileDeletePending must
// block a subsequent open of its base file however that open spells the path.
//
// The stream handle is built with the name triple CREATE records for
// "d/f:s1" — a client-spelled Path alongside the resolved parent handle and
// parent-relative name.
func TestIsFileOrBaseDeletePending_SpellingIndependent(t *testing.T) {
	parent := []byte{0xAA, 0xBB}
	baseHandle := []byte{0x01}
	streamHandle := []byte{0x02}

	for _, storedPath := range []string{"d/f:s1", "d/sub/../f:s1", "d//f:s1", "d/./f:s1"} {
		t.Run(storedPath, func(t *testing.T) {
			h := NewHandler()

			stream := (&OpenFile{
				FileID:         h.GenerateFileID(),
				MetadataHandle: streamHandle,
			}).WithName(OpenName{
				Path:         storedPath,
				FileName:     "f:s1",
				ParentHandle: parent,
			})
			stream.BaseFileDeletePending = true
			h.StoreOpenFile(stream)

			if !h.isFileOrBaseDeletePending(baseHandle, parent, "f") {
				t.Errorf("stored path %q: base open not refused, "+
					"want DELETE_PENDING from the stream handle", storedPath)
			}
		})
	}

	// A same-named file in a different directory must not be dragged in.
	t.Run("different parent", func(t *testing.T) {
		h := NewHandler()
		stream := (&OpenFile{
			FileID:         h.GenerateFileID(),
			MetadataHandle: streamHandle,
		}).WithName(OpenName{
			Path:         "other/f:s1",
			FileName:     "f:s1",
			ParentHandle: []byte{0xCC, 0xDD},
		})
		stream.BaseFileDeletePending = true
		h.StoreOpenFile(stream)

		if h.isFileOrBaseDeletePending(baseHandle, parent, "f") {
			t.Error("a stream in a different directory must not make this base delete-pending")
		}
	})
}
