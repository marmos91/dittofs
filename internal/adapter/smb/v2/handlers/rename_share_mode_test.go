package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestCheckRenameDstParentShareMode verifies the dst-parent share-mode
// predicate that gates SMB rename per MS-FSA §2.1.5.1.2 + §2.1.5.14.10
// (mirroring Samba's `smbd_smb2_setinfo_rename_dst_parent_check`). Rename
// of a child mutates the parent dir's contents, so any handle on the
// parent that holds DELETE access OR was opened without FILE_SHARE_WRITE
// must produce STATUS_SHARING_VIOLATION.
//
// The matrix mirrors the four smbtorture smb2.rename subtests under #433:
//
//	parent-share | parent-DELETE | expected
//	-------------+---------------+----------
//	R|W|D        | yes           | conflict   (share_delete_and_delete_access)
//	0            | yes           | conflict   (no_share_delete_but_delete_access)
//	R|W|D        | no            | no-conflict (share_delete_no_delete_access — passing)
//	0            | no            | conflict   (no_share_delete_no_delete_access)
func TestCheckRenameDstParentShareMode(t *testing.T) {
	const (
		shareRWD     = uint32(types.FileShareRead | types.FileShareWrite | types.FileShareDelete)
		shareNone    = uint32(0)
		accessDelete = uint32(types.Delete | types.FileReadAttributes)
		accessNoDel  = uint32(types.FileReadAttributes | types.FileWriteData)
	)

	parentMeta := metadata.FileHandle("parent-dir")
	otherMeta := metadata.FileHandle("file.txt")

	t.Run("ShareDeleteAndDeleteAccess_Conflict", func(t *testing.T) {
		h := NewHandler()
		// Open on parent dir with R|W|D share + DELETE access (smbtorture
		// share_delete_and_delete_access dh).
		dh := &OpenFile{
			FileID:         h.GenerateFileID(),
			MetadataHandle: parentMeta,
			ShareAccess:    shareRWD,
			DesiredAccess:  accessDelete,
		}
		h.StoreOpenFile(dh)
		// Renamer: handle on the file itself, parent=parentMeta.
		fhID := h.GenerateFileID()
		if !h.checkRenameDstParentShareMode(parentMeta, fhID) {
			t.Fatalf("expected SHARING_VIOLATION (parent has DELETE access)")
		}
	})

	t.Run("NoShareDeleteButDeleteAccess_Conflict", func(t *testing.T) {
		h := NewHandler()
		dh := &OpenFile{
			FileID:         h.GenerateFileID(),
			MetadataHandle: parentMeta,
			ShareAccess:    shareNone,
			DesiredAccess:  accessDelete,
		}
		h.StoreOpenFile(dh)
		fhID := h.GenerateFileID()
		if !h.checkRenameDstParentShareMode(parentMeta, fhID) {
			t.Fatalf("expected SHARING_VIOLATION (parent has DELETE access, no FILE_SHARE_WRITE)")
		}
	})

	t.Run("ShareDeleteNoDeleteAccess_NoConflict", func(t *testing.T) {
		h := NewHandler()
		dh := &OpenFile{
			FileID:         h.GenerateFileID(),
			MetadataHandle: parentMeta,
			ShareAccess:    shareRWD,
			DesiredAccess:  accessNoDel,
		}
		h.StoreOpenFile(dh)
		fhID := h.GenerateFileID()
		if h.checkRenameDstParentShareMode(parentMeta, fhID) {
			t.Fatalf("did not expect conflict (parent permits FILE_SHARE_WRITE and no DELETE access)")
		}
	})

	t.Run("NoShareDeleteNoDeleteAccess_Conflict", func(t *testing.T) {
		h := NewHandler()
		dh := &OpenFile{
			FileID:         h.GenerateFileID(),
			MetadataHandle: parentMeta,
			ShareAccess:    shareNone,
			DesiredAccess:  accessNoDel,
		}
		h.StoreOpenFile(dh)
		fhID := h.GenerateFileID()
		if !h.checkRenameDstParentShareMode(parentMeta, fhID) {
			t.Fatalf("expected SHARING_VIOLATION (parent share_access excludes FILE_SHARE_WRITE)")
		}
	})

	t.Run("ExcludesRenamerOwnHandle", func(t *testing.T) {
		// A directory rename via the directory's own handle: the renamer's
		// FileID must be excluded from the conflict check, otherwise a
		// solo dir-rename would always self-block.
		h := NewHandler()
		ownID := h.GenerateFileID()
		dh := &OpenFile{
			FileID:         ownID,
			MetadataHandle: parentMeta,
			ShareAccess:    shareNone,
			DesiredAccess:  accessDelete,
		}
		h.StoreOpenFile(dh)
		if h.checkRenameDstParentShareMode(parentMeta, ownID) {
			t.Fatalf("expected no conflict when only the renamer's own handle is on the parent")
		}
	})

	t.Run("EmptyParentHandle_NoConflict", func(t *testing.T) {
		h := NewHandler()
		dh := &OpenFile{
			FileID:         h.GenerateFileID(),
			MetadataHandle: parentMeta,
			ShareAccess:    shareNone,
			DesiredAccess:  accessDelete,
		}
		h.StoreOpenFile(dh)
		if h.checkRenameDstParentShareMode(metadata.FileHandle{}, h.GenerateFileID()) {
			t.Fatalf("expected no conflict for empty parent handle")
		}
	})

	t.Run("UnrelatedHandle_NotCounted", func(t *testing.T) {
		// A handle on a different file must not be counted toward the
		// parent-dir share-mode check.
		h := NewHandler()
		other := &OpenFile{
			FileID:         h.GenerateFileID(),
			MetadataHandle: otherMeta,
			ShareAccess:    shareNone,
			DesiredAccess:  accessDelete,
		}
		h.StoreOpenFile(other)
		if h.checkRenameDstParentShareMode(parentMeta, h.GenerateFileID()) {
			t.Fatalf("unrelated handle on different metadata must not trigger conflict")
		}
	})
}
