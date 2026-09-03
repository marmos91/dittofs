// Coverage for restoreParentDirFrozenTimestamps when the freeze and the child
// operation belong to DIFFERENT principals. The freeze belongs to whoever holds
// the open directory handle and set the -1 sentinel on it; the restore is
// driven by another user's child create / rename / delete. Running the restore
// as the child-operation's caller makes a directory's frozen timestamps
// survive or vanish according to who owns the directory.
package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// frozenDirOwnerUID owns the fixture directory and opened the freezing handle.
// It is neither root nor the session caller the fixture's handler serves.
const frozenDirOwnerUID = uint32(2002)

// authContextForUID builds an AuthContext for a bare UID, with the GID equal to it.
func authContextForUID(uid uint32) *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &uid},
	}
}

// frozenDirFixture builds a directory owned by frozenDirOwnerUID with its
// timestamps pinned to a known value, plus a directory OpenFile opened by that
// same UID with the given GrantedAccess and all four timestamps frozen on it.
// The returned handler's session caller is neither the directory's owner nor
// the handle's opener.
func frozenDirFixture(t *testing.T, grantedAccess uint32) (
	h *Handler,
	ctx *SMBHandlerContext,
	dirHandle metadata.FileHandle,
	pinned time.Time,
) {
	t.Helper()

	h, ctx, rootHandle := setupStreamsDisabledShare(t, false)

	rootCtx := authContextForUID(0)
	metaSvc := h.Registry.GetMetadataService()

	// Mode 0o777 so the session caller may create a child in it: the child
	// operation must succeed, or the restore would never be reached.
	dir, _, err := metaSvc.CreateDirectory(rootCtx, rootHandle, "frozendir", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
		UID:  frozenDirOwnerUID,
		GID:  frozenDirOwnerUID,
	})
	if err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	dirHandle, err = metadata.EncodeFileHandle(dir)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	pinned = time.Date(2029, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err := metaSvc.SetFileAttributes(rootCtx, dirHandle, &metadata.SetAttrs{
		Mtime: &pinned, Ctime: &pinned, Atime: &pinned, CreationTime: &pinned,
	}); err != nil {
		t.Fatalf("pin directory timestamps: %v", err)
	}

	openerUID, openerGID := frozenDirOwnerUID, frozenDirOwnerUID
	dirOpen := (&OpenFile{
		FileID:         [16]byte{0xD1},
		MetadataHandle: dirHandle,
		ShareName:      ctx.ShareName,
		SessionID:      ctx.SessionID,
		TreeID:         ctx.TreeID,
		IsDirectory:    true,
		DesiredAccess:  grantedAccess,
		GrantedAccess:  grantedAccess,
		OpenerUser: &models.User{
			Username: "dir-owner",
			UID:      &openerUID,
			Groups:   []models.Group{{GID: &openerGID}},
		},
		MtimeFrozen: true, FrozenMtime: &pinned,
		CtimeFrozen: true, FrozenCtime: &pinned,
		AtimeFrozen: true, FrozenAtime: &pinned,
		BtimeFrozen: true, FrozenBtime: &pinned,
	}).WithName(OpenName{Path: "frozendir", FileName: "frozendir", ParentHandle: rootHandle})
	h.StoreOpenFile(dirOpen)

	return h, ctx, dirHandle, pinned
}

// bumpDirTimestamps moves the directory's stored Mtime/Ctime off the frozen
// value, standing in for the unconditional parent-directory bump a child
// create / rename / delete performs. It is driven directly because the metadata
// layer coalesces that bump, so it is not yet in the store when the restore
// runs — and the restore's authorization, not the bump's timing, is what is
// under test here.
func bumpDirTimestamps(t *testing.T, h *Handler, dirHandle metadata.FileHandle) time.Time {
	t.Helper()
	bumped := time.Date(2032, 8, 9, 10, 11, 12, 0, time.UTC)
	if _, err := h.Registry.GetMetadataService().SetFileAttributes(authContextForUID(0), dirHandle,
		&metadata.SetAttrs{Mtime: &bumped, Ctime: &bumped}); err != nil {
		t.Fatalf("bump directory timestamps: %v", err)
	}
	return bumped
}

// TestRestoreParentDirFrozenTimestamps_NonOwningChildCaller asserts a
// directory's frozen timestamps survive a child operation driven by a caller
// who neither owns the directory nor opened the freezing handle. The restore is
// authorized by the freezing handle's FILE_WRITE_ATTRIBUTES grant, so it no
// longer turns on the caller's relationship to the directory.
func TestRestoreParentDirFrozenTimestamps_NonOwningChildCaller(t *testing.T) {
	h, ctx, dirHandle, pinned := frozenDirFixture(t,
		uint32(types.FileWriteAttributes)|uint32(types.FileReadAttributes))

	metaSvc := h.Registry.GetMetadataService()
	callerCtx, err := BuildAuthContext(ctx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}
	if uid := callerCtx.Identity.UID; uid == nil || *uid == frozenDirOwnerUID || *uid == 0 {
		t.Fatalf("precondition: caller must be a non-owning, non-root principal, got UID %v", uid)
	}
	// The caller must be able to write in the directory, or the child operation
	// that drives the restore could never have happened.
	if _, _, err := metaSvc.CreateFile(callerCtx, dirHandle, "child.dat", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	}); err != nil {
		t.Fatalf("CreateFile(child) as the non-owning caller: %v", err)
	}

	bumped := bumpDirTimestamps(t, h, dirHandle)
	h.restoreParentDirFrozenTimestamps(callerCtx, dirHandle)

	got, err := metaSvc.GetFile(context.Background(), dirHandle)
	if err != nil {
		t.Fatalf("GetFile(dir): %v", err)
	}
	if got.Mtime.Equal(bumped) {
		t.Errorf("directory Mtime is still the bumped %v after the restore; the frozen %v was lost "+
			"because the restore was gated on the non-owning caller", bumped.UTC(), pinned)
	} else if !got.Mtime.Equal(pinned) {
		t.Errorf("directory Mtime = %v; want the frozen %v", got.Mtime.UTC(), pinned)
	}
	if !got.Ctime.Equal(pinned) {
		t.Errorf("directory Ctime = %v; want the frozen %v", got.Ctime.UTC(), pinned)
	}
}

// TestRestoreParentDirFrozenTimestamps_GrantIsWhatAuthorizes is the control
// that pins WHY the previous case now works. The same non-owning caller drives
// the same restore, but the directory handle carries no FILE_WRITE_ATTRIBUTES,
// and the restore is refused — so the fix is keyed on the handle's access
// right, not a blanket bypass of the metadata layer's ownership rule.
//
// The combination cannot occur through the protocol: the frozen flags are only
// ever set by SET_INFO FileBasicInformation, which is itself gated on
// FILE_WRITE_ATTRIBUTES, so a frozen handle always carries the right. It is
// constructed here precisely because that is what makes it a control.
func TestRestoreParentDirFrozenTimestamps_GrantIsWhatAuthorizes(t *testing.T) {
	h, ctx, dirHandle, _ := frozenDirFixture(t, uint32(types.FileReadAttributes))

	callerCtx, err := BuildAuthContext(ctx)
	if err != nil {
		t.Fatalf("BuildAuthContext: %v", err)
	}

	bumped := bumpDirTimestamps(t, h, dirHandle)
	h.restoreParentDirFrozenTimestamps(callerCtx, dirHandle)

	got, err := h.Registry.GetMetadataService().GetFile(context.Background(), dirHandle)
	if err != nil {
		t.Fatalf("GetFile(dir): %v", err)
	}
	if !got.Mtime.Equal(bumped) {
		t.Errorf("directory Mtime = %v without FILE_WRITE_ATTRIBUTES on the frozen handle; "+
			"want the un-restored %v — the restore must be authorized by the grant, not unconditionally",
			got.Mtime.UTC(), bumped.UTC())
	}
}

// TestRestoreParentDirFrozenTimestamps_OwningChildCaller is the compatibility
// control: the restore driven by a caller who owns the directory already
// worked, and must keep working.
func TestRestoreParentDirFrozenTimestamps_OwningChildCaller(t *testing.T) {
	h, _, dirHandle, pinned := frozenDirFixture(t,
		uint32(types.FileWriteAttributes)|uint32(types.FileReadAttributes))

	bumpDirTimestamps(t, h, dirHandle)
	h.restoreParentDirFrozenTimestamps(authContextForUID(frozenDirOwnerUID), dirHandle)

	got, err := h.Registry.GetMetadataService().GetFile(context.Background(), dirHandle)
	if err != nil {
		t.Fatalf("GetFile(dir): %v", err)
	}
	if !got.Mtime.Equal(pinned) {
		t.Errorf("directory Mtime = %v after the owner drove the restore; want the frozen %v",
			got.Mtime.UTC(), pinned)
	}
}
