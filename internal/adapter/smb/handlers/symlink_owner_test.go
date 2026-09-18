package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// rehomePlaceholder rewrites the "link" entry under rootHandle to the given
// owner/mode, so a test can model a placeholder that some other principal
// created. Both symlink-conversion paths remove-and-recreate that entry, and
// must not change who owns it.
func rehomePlaceholder(t *testing.T, h *Handler, smbCtx *SMBHandlerContext, rootHandle metadata.FileHandle, uid, gid, mode uint32) {
	t.Helper()
	metaSvc := h.Registry.GetMetadataService()
	childHandle, err := metaSvc.GetChild(smbCtx.Context, rootHandle, "link")
	if err != nil {
		t.Fatalf("GetChild(link): %v", err)
	}
	rootUID, rootGID := uint32(0), uint32(0)
	rootCtx := &metadata.AuthContext{
		Context:  smbCtx.Context,
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID},
	}
	if _, err := metaSvc.SetFileAttributes(rootCtx, childHandle, &metadata.SetAttrs{
		UID: &uid, GID: &gid, Mode: &mode,
	}); err != nil {
		t.Fatalf("SetFileAttributes(rehome): %v", err)
	}
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
