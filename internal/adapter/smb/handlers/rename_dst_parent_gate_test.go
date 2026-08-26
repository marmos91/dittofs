// Destination-parent share-mode gate for SET_INFO rename.
//
// MS-FSA 2.1.5.15.12 ("FileRenameInformation") opens the destination directory with
// DesiredAccess FILE_ADD_FILE|SYNCHRONIZE and ShareAccess
// FILE_SHARE_READ|FILE_SHARE_WRITE. The cases below pin both directions of
// that pairing: a write-sharing holder must not block the rename, and a
// DELETE-access holder must still block it.

package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

const (
	secFileAll   = uint32(0x000001FF) // every file-specific right, no DELETE
	deleteAccess = uint32(types.Delete)
	readAttrs    = uint32(types.FileReadAttributes)
)

// storeDstParentHolder registers an open handle on dstParent and returns it.
func storeDstParentHolder(h *Handler, dstParent metadata.FileHandle, desired, share uint32) *OpenFile {
	of := (&OpenFile{
		FileID:         h.GenerateFileID(),
		MetadataHandle: dstParent,
		DesiredAccess:  desired,
		ShareAccess:    share,
		ShareName:      "share",
		SessionID:      25,
		TreeID:         25,
	}).WithName(OpenName{Path: "watched-dir"})
	h.StoreOpenFile(of)
	return of
}

// storeRenamer registers the source-file handle that the rename runs on.
func storeRenamer(h *Handler) *OpenFile {
	of := (&OpenFile{
		FileID:         h.GenerateFileID(),
		MetadataHandle: metadata.FileHandle{0x51, 0x52},
		DesiredAccess:  secFileAll | deleteAccess,
		ShareAccess:    smbShareRead | smbShareWrite | smbShareDelete,
		ShareName:      "share",
		SessionID:      7,
		TreeID:         7,
	}).WithName(OpenName{Path: "watched-dir/subdir/file", ParentHandle: metadata.FileHandle{0x99}})
	h.StoreOpenFile(of)
	return of
}

func TestParentDirRenameConflict(t *testing.T) {
	dstParent := metadata.FileHandle{0xDE, 0xAD, 0xBE, 0xEF}

	tests := []struct {
		name          string
		holderDesired uint32
		holderShare   uint32
		wantConflict  bool
	}{
		{
			// A CHANGE_NOTIFY watcher on the destination directory:
			// SEC_FILE_ALL with FILE_SHARE_READ|FILE_SHARE_WRITE. It permits
			// write sharing and holds no DELETE, so linking a new name into
			// the directory does not conflict with it.
			name:          "write-sharing watcher does not block the rename",
			holderDesired: secFileAll,
			holderShare:   smbShareRead | smbShareWrite,
			wantConflict:  false,
		},
		{
			// The rename's implicit open withholds share-delete, so a holder
			// that already has DELETE access on the destination parent is
			// incompatible and must still be refused.
			name:          "delete-access holder still blocks the rename",
			holderDesired: deleteAccess,
			holderShare:   smbShareRead | smbShareWrite | smbShareDelete,
			wantConflict:  true,
		},
		{
			// The implicit open requests FILE_ADD_FILE, a write against the
			// directory, so a holder denying write sharing must be refused.
			name:          "holder denying write sharing still blocks the rename",
			holderDesired: secFileAll,
			holderShare:   smbShareRead | smbShareDelete,
			wantConflict:  true,
		},
		{
			// Stat-only opens impose no share-mode constraint at all.
			name:          "stat-only holder does not block the rename",
			holderDesired: readAttrs,
			holderShare:   0,
			wantConflict:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler()
			renamer := storeRenamer(h)
			storeDstParentHolder(h, dstParent, tc.holderDesired, tc.holderShare)

			if got := h.checkParentDirRenameConflict(renamer, dstParent); got != tc.wantConflict {
				t.Fatalf("conflict = %v, want %v", got, tc.wantConflict)
			}
		})
	}
}

// The renamer's own handle must never be its own conflict. Exercised by
// pointing the renamer's handle at the destination parent itself, the only
// shape in which the scan can reach it.
func TestParentDirRenameConflict_ExcludesRenamerOwnHandle(t *testing.T) {
	dstParent := metadata.FileHandle{0xDE, 0xAD, 0xBE, 0xEF}
	h := NewHandler()

	renamer := (&OpenFile{
		FileID:         h.GenerateFileID(),
		MetadataHandle: dstParent,
		DesiredAccess:  deleteAccess, // would trip the gate as any other holder
		ShareAccess:    0,
		ShareName:      "share",
		SessionID:      7,
		TreeID:         7,
	}).WithName(OpenName{Path: "watched-dir"})
	h.StoreOpenFile(renamer)

	if h.checkParentDirRenameConflict(renamer, dstParent) {
		t.Fatal("renamer blocked by its own open handle on the destination parent")
	}
}

// A second handle held by the renamer's own session is not excluded: the
// implicit destination-parent open is evaluated against the whole open list,
// so a same-session DELETE-access holder still refuses the rename.
func TestParentDirRenameConflict_SameSessionHolderStillConflicts(t *testing.T) {
	dstParent := metadata.FileHandle{0xDE, 0xAD, 0xBE, 0xEF}
	h := NewHandler()

	renamer := storeRenamer(h)
	holder := storeDstParentHolder(h, dstParent, deleteAccess, smbShareRead|smbShareWrite|smbShareDelete)
	holder.SessionID = renamer.SessionID
	holder.TreeID = renamer.TreeID

	if !h.checkParentDirRenameConflict(renamer, dstParent) {
		t.Fatal("same-session DELETE-access holder did not refuse the rename")
	}
}
