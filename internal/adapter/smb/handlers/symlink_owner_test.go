package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// rootCtxFor builds the root metadata context the fixture's setup helpers use.
func rootCtxFor(smbCtx *SMBHandlerContext) *metadata.AuthContext {
	rootUID, rootGID := uint32(0), uint32(0)
	return &metadata.AuthContext{
		Context:  smbCtx.Context,
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID},
	}
}

// rehomeAttr applies attrs to handle as root, for tests that need to set up
// state the SMB handler itself cannot reach.
func rehomeAttr(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, handle metadata.FileHandle, attrs *metadata.SetAttrs) {
	t.Helper()
	if _, err := h.Registry.GetMetadataService().SetFileAttributes(rootCtxFor(smbCtx), handle, attrs); err != nil {
		t.Fatalf("SetFileAttributes: %v", err)
	}
}

// rehomePlaceholder rewrites the "link" entry under rootHandle to the given
// owner/mode, so a test can model a placeholder that some other principal
// created. Both symlink-conversion paths remove-and-recreate that entry, and
// must not change who owns it.
func rehomePlaceholder(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, rootHandle metadata.FileHandle, uid, gid, mode uint32) {
	t.Helper()
	childHandle, err := h.Registry.GetMetadataService().GetChild(smbCtx.Context, rootHandle, "link")
	if err != nil {
		t.Fatalf("GetChild(link): %v", err)
	}
	rehomeAttr(t, h, smbCtx, childHandle, &metadata.SetAttrs{UID: &uid, GID: &gid, Mode: &mode})
}

// entryAttrAfter resolves the "link" entry under rootHandle and returns its
// full attributes. Both conversion paths leave the entry resolvable by name.
func entryAttrAfter(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, rootHandle metadata.FileHandle) *metadata.File {
	t.Helper()
	metaSvc := h.Registry.GetMetadataService()
	childHandle, err := metaSvc.GetChild(smbCtx.Context, rootHandle, "link")
	if err != nil {
		t.Fatalf("GetChild(link): %v", err)
	}
	file, err := metaSvc.GetFile(smbCtx.Context, childHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	return file
}

// TestCarriedAttr covers the shared helper both conversion paths call. It is
// what makes the rollback branch testable: the branch is unreachable through
// the handlers without fault injection, because the same auth context that
// authorizes the remove also authorizes the recreate.
func TestCarriedAttr(t *testing.T) {
	src := &metadata.File{FileAttr: metadata.FileAttr{UID: 4242, GID: 4343, Mode: 0o600}}

	t.Run("carries the owner", func(t *testing.T) {
		got := carriedAttr(src, &metadata.FileAttr{})
		if got.UID != 4242 || got.GID != 4343 {
			t.Errorf("owner = %d:%d, want 4242:4343", got.UID, got.GID)
		}
	})

	t.Run("leaves the mode alone", func(t *testing.T) {
		got := carriedAttr(src, &metadata.FileAttr{})
		if got.Mode != 0 {
			t.Errorf("mode = 0o%o, want 0 (symlink default applies)", got.Mode)
		}
	})

	t.Run("preserves the caller's type", func(t *testing.T) {
		got := carriedAttr(src, &metadata.FileAttr{Type: metadata.FileTypeRegular})
		if got.Type != metadata.FileTypeRegular {
			t.Errorf("type = %v, want FileTypeRegular", got.Type)
		}
	})

	t.Run("nil source leaves dst untouched", func(t *testing.T) {
		got := carriedAttr(nil, &metadata.FileAttr{UID: 7, GID: 8})
		if got.UID != 7 || got.GID != 8 {
			t.Errorf("owner = %d:%d, want 7:8 untouched", got.UID, got.GID)
		}
		if got.ExactAttrs {
			t.Error("ExactAttrs set with no source; the create would take root's 0:0 as exact")
		}
	})
}

// TestSetReparsePoint_PreservesPlaceholderOwner pins that converting a
// placeholder into a symlink does not re-home it. The conversion replaces the
// same object, so the symlink must keep the placeholder's owner even though the
// caller converting it is a different principal (the fixture's caller is
// uid/gid 1000).
func TestSetReparsePoint_PreservesPlaceholderOwner(t *testing.T) {
	const ownerUID, ownerGID uint32 = 4242, 4343

	h, smbCtx, rootHandle, fileID := setupReparseShare(t)
	rehomePlaceholder(t, h, smbCtx, rootHandle, ownerUID, ownerGID, 0o600)

	body := buildSetReparseBody(fileID, buildSymlinkReparseBuffer("../A"))
	resp, err := h.handleSetReparsePoint(smbCtx, body)
	if err != nil {
		t.Fatalf("handleSetReparsePoint: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%08x, want STATUS_SUCCESS", uint32(resp.Status))
	}

	after := entryAttrAfter(t, h, smbCtx, rootHandle)
	if after.Type != metadata.FileTypeSymlink {
		t.Fatalf("type = %v, want FileTypeSymlink", after.Type)
	}
	if after.UID != ownerUID || after.GID != ownerGID {
		t.Errorf("owner = %d:%d after SET_REPARSE_POINT; want %d:%d (placeholder's owner) unchanged",
			after.UID, after.GID, ownerUID, ownerGID)
	}
}

// TestClose_MFsymlink_PreservesPlaceholderOwner is the CLOSE-path sibling of
// the reparse test: promoting an MFsymlink must not re-home it either.
func TestClose_MFsymlink_PreservesPlaceholderOwner(t *testing.T) {
	const ownerUID, ownerGID uint32 = 4242, 4343

	h, smbCtx, rootHandle, fileID := setupMFsymlinkShare(t, true, "valid/target")
	rehomePlaceholder(t, h, smbCtx, rootHandle, ownerUID, ownerGID, 0o600)

	if got := fileTypeAfterClose(t, h, smbCtx, rootHandle, fileID); got != metadata.FileTypeSymlink {
		t.Fatalf("file type after close = %v, expected FileTypeSymlink (conversion did not fire)", got)
	}

	after := entryAttrAfter(t, h, smbCtx, rootHandle)
	if after.UID != ownerUID || after.GID != ownerGID {
		t.Errorf("owner = %d:%d after MFsymlink promotion; want %d:%d (placeholder's owner) unchanged",
			after.UID, after.GID, ownerUID, ownerGID)
	}
}

// TestSetReparsePoint_PreservesOwnerUnderSGIDParent covers the SGID case: the
// create path inherits a new entry's group from an SGID parent, which would
// override the placeholder's own group on the re-create. The conversion must
// keep the group the placeholder actually had.
func TestSetReparsePoint_PreservesOwnerUnderSGIDParent(t *testing.T) {
	const ownerUID, ownerGID uint32 = 4242, 5555

	h, smbCtx, rootHandle, fileID := setupReparseShare(t)

	// Make the parent SGID-owned by a group the placeholder does not belong to,
	// so inheritance and preservation give different answers.
	sgidParent := uint32(0o2777)
	parentGID := uint32(7777)
	rehomeAttr(t, h, smbCtx, rootHandle, &metadata.SetAttrs{Mode: &sgidParent, GID: &parentGID})

	rehomePlaceholder(t, h, smbCtx, rootHandle, ownerUID, ownerGID, 0o600)

	body := buildSetReparseBody(fileID, buildSymlinkReparseBuffer("../A"))
	resp, err := h.handleSetReparsePoint(smbCtx, body)
	if err != nil {
		t.Fatalf("handleSetReparsePoint: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%08x, want STATUS_SUCCESS", uint32(resp.Status))
	}

	after := entryAttrAfter(t, h, smbCtx, rootHandle)
	if after.UID != ownerUID || after.GID != ownerGID {
		t.Errorf("owner = %d:%d under an SGID parent; want %d:%d (placeholder's own, not the parent's %d)",
			after.UID, after.GID, ownerUID, ownerGID, parentGID)
	}
}

// TestSetReparsePoint_RollbackAttrRestoresZeroMode is a helper-contract test,
// not an end-to-end one. It re-creates exactly what the rollback branch does
// and asserts the result keeps an explicit mode 0. The branch itself is
// unreachable through the handlers without fault injection: the same auth
// context authorizes the RemoveFile and the CreateSymlink, and ACE4_ADD_FILE is
// the same bit as ACE4_WRITE_DATA, so no parent DACL can deny one and allow the
// other. Kept because the rule it pins — a re-create must not let
// ApplyModeDefault read 0 as "unspecified" — is the one the rollback depends on.
func TestSetReparsePoint_RollbackAttrRestoresZeroMode(t *testing.T) {
	h, smbCtx, rootHandle, _ := setupReparseShare(t)
	metaSvc := h.Registry.GetMetadataService()

	childHandle, err := metaSvc.GetChild(smbCtx.Context, rootHandle, "link")
	if err != nil {
		t.Fatalf("GetChild(link): %v", err)
	}
	zero := uint32(0)
	rehomeAttr(t, h, smbCtx, childHandle, &metadata.SetAttrs{Mode: &zero})
	pre, err := metaSvc.GetFile(smbCtx.Context, childHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if pre.Mode&0o7777 != 0 {
		t.Fatalf("precondition: placeholder mode = 0o%o, want 0", pre.Mode&0o7777)
	}

	// Re-create exactly what the rollback branch does.
	rollback := carriedAttr(pre, &metadata.FileAttr{Type: metadata.FileTypeRegular})
	rollback.Mode = pre.Mode
	if _, _, err := metaSvc.CreateFile(rootCtxFor(smbCtx), rootHandle, "rolledback", rollback); err != nil {
		t.Fatalf("CreateFile(rollback): %v", err)
	}

	rb, err := metaSvc.GetChild(smbCtx.Context, rootHandle, "rolledback")
	if err != nil {
		t.Fatalf("GetChild(rolledback): %v", err)
	}
	got, err := metaSvc.GetFile(smbCtx.Context, rb)
	if err != nil {
		t.Fatalf("GetFile(rolledback): %v", err)
	}
	if got.Mode&0o7777 != 0 {
		t.Errorf("rollback mode = 0o%o; want the placeholder's explicit 0 preserved", got.Mode&0o7777)
	}
	if got.UID != pre.UID || got.GID != pre.GID {
		t.Errorf("rollback owner = %d:%d; want %d:%d unchanged", got.UID, got.GID, pre.UID, pre.GID)
	}
}

// TestExactAttrs_NotPersisted pins that the create-path marker does not leak
// into stored state. The memory backend keeps the whole FileAttr, so a marker
// left set would come back through GetFile and let a later caller take the
// exact-create path by accident.
func TestExactAttrs_NotPersisted(t *testing.T) {
	h, smbCtx, rootHandle, _ := setupReparseShare(t)
	metaSvc := h.Registry.GetMetadataService()

	_, _, err := metaSvc.CreateFile(rootCtxFor(smbCtx), rootHandle, "exact", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o600, ExactAttrs: true,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	fh, err := metaSvc.GetChild(smbCtx.Context, rootHandle, "exact")
	if err != nil {
		t.Fatalf("GetChild: %v", err)
	}
	got, err := metaSvc.GetFile(smbCtx.Context, fh)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if got.ExactAttrs {
		t.Errorf("ExactAttrs = true in stored state; it is a create-path instruction, not file state")
	}
}
