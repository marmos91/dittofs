// Handler-level coverage for the timestamps a SET_INFO rename leaves behind.
//
// A client that renames through a handle it still holds open must keep
// observing the ChangeTime that handle was handed in its CREATE reply.
// LastModificationTime is left alone either way. A rename is authorized on the
// parent directory, so both must hold for a caller who may rename the file
// without being able to write that file's own attributes.
package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// setupRenameTimestampFixture reuses the BasicInfo timestamp share: it grants
// the handle DELETE (which is what gates rename) and a tree connection (a
// rename to a share-root-relative path resolves the root through it), hands
// the file to fileUID/fileGID, and backdates its timestamps so any advance the
// rename makes is unambiguous rather than sub-millisecond. Returns the
// handler, an auth context for the calling identity, the open handle, and the
// seeded time.
func setupRenameTimestampFixture(t *testing.T, fileUID, fileGID, callerUID, callerGID uint32) (
	*Handler, *metadata.AuthContext, *OpenFile, time.Time,
) {
	t.Helper()

	h, rootCtx, fileHandle, open := setupBasicInfoTimestampTest(t)
	access := uint32(types.FileReadAttributes | types.FileWriteAttributes | types.Delete)
	open.DesiredAccess, open.GrantedAccess = access, access
	open.TreeID = 1
	h.StoreTree(&TreeConnection{
		TreeID:     open.TreeID,
		ShareName:  open.ShareName,
		Permission: models.PermissionReadWrite,
	})

	past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if _, err := h.Registry.GetMetadataService().SetFileAttributes(rootCtx, fileHandle, &metadata.SetAttrs{
		UID: &fileUID, GID: &fileGID, Mtime: &past, Ctime: &past,
	}); err != nil {
		t.Fatalf("seed owner + timestamps: %v", err)
	}

	callerCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &callerUID, GID: &callerGID},
	}
	return h, callerCtx, open, past
}

// TestSetInfo_Rename_ChangeTimePreservedForEveryCaller pins that a rename
// leaves the ChangeTime a client already observed alone, and that it does so
// regardless of whether the caller could have written the file's timestamps
// directly. The two cases differ only in file ownership: both may rename (the
// parent directory is 0o777), only the owner may set the file's timestamps
// explicitly. The preserve must not turn on that difference: one rename that
// reports two different ChangeTimes depending on who ran it is a defect in its
// own right.
func TestSetInfo_Rename_ChangeTimePreservedForEveryCaller(t *testing.T) {
	cases := []struct {
		name                                   string
		fileUID, fileGID, callerUID, callerGID uint32
		callerMayWriteTimestamps               bool
	}{
		{"caller owns the file", 1000, 1000, 1000, 1000, true},
		{"caller does not own the file", 1000, 1000, 2000, 2000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, authCtx, open, past := setupRenameTimestampFixture(t,
				tc.fileUID, tc.fileGID, tc.callerUID, tc.callerGID)
			metaSvc := h.Registry.GetMetadataService()

			// Establish that the two cases really do differ in the way the
			// defect turned on, or they are not measuring anything.
			probe := past
			_, wErr := metaSvc.SetFileAttributes(authCtx, open.MetadataHandle, &metadata.SetAttrs{Ctime: &probe})
			if tc.callerMayWriteTimestamps != (wErr == nil) {
				t.Fatalf("precondition: caller timestamp write err=%v, want permitted=%v",
					wErr, tc.callerMayWriteTimestamps)
			}

			resp, err := h.setFileInfoFromStore(
				nil, authCtx, open, types.FileRenameInformation, encodeRenameInfo("renamed.txt"))
			if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
				t.Fatalf("rename: err=%v resp=%v", err, resp)
			}

			after, err := metaSvc.GetFile(context.Background(), open.MetadataHandle)
			if err != nil {
				t.Fatalf("GetFile after rename: %v", err)
			}
			if !after.Ctime.Equal(past) {
				t.Errorf("ChangeTime = %v after rename; want %v unchanged for a handle held open across it",
					after.Ctime.UTC(), past.UTC())
			}
			if !after.Mtime.Equal(past) {
				t.Errorf("LastWriteTime = %v after rename; want %v unchanged",
					after.Mtime.UTC(), past.UTC())
			}
		})
	}
}

// TestSetInfo_Rename_FrozenChangeTimeSurvivesRename covers the -1 sentinel on
// the same path: a handle that froze ChangeTime must not see it move either.
// Reads the store directly rather than closing the handle, because CLOSE runs
// its own frozen-timestamp restore and would mask whether the rename path did
// anything.
func TestSetInfo_Rename_FrozenChangeTimeSurvivesRename(t *testing.T) {
	// Only an owner can freeze a timestamp — the sentinel pins the pre-image
	// through the same ownership-gated write the restore uses — so there is no
	// non-owning variant of this case to cover.
	h, authCtx, open, past := setupRenameTimestampFixture(t, 1000, 1000, 1000, 1000)
	metaSvc := h.Registry.GetMetadataService()

	freeze := encodeBasicInfo(0, 0, 0, filetimeFreeze, 0)
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileBasicInformation, freeze)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("freeze SET_INFO: err=%v resp=%v", err, resp)
	}
	if !open.CtimeFrozen {
		t.Fatal("sentinel did not freeze ChangeTime; the assertion below would prove nothing")
	}

	resp, err = h.setFileInfoFromStore(nil, authCtx, open, types.FileRenameInformation, encodeRenameInfo("renamed.txt"))
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("rename: err=%v resp=%v", err, resp)
	}

	after, err := metaSvc.GetFile(context.Background(), open.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile after rename: %v", err)
	}
	if !after.Ctime.Equal(past) {
		t.Errorf("ChangeTime = %v after renaming a handle with ChangeTime frozen; want the frozen %v",
			after.Ctime.UTC(), past.UTC())
	}
}
