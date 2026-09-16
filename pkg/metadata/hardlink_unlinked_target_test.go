package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestCreateHardLink_RefusesUnlinkedTarget pins that a hard link cannot be
// created to an inode whose last name has already been removed.
//
// RemoveFile keeps the inode row after the final unlink so an open descriptor
// can still stat it, and it hands the caller a non-empty PayloadID meaning the
// content may be freed. A link created afterwards would resolve to blocks that
// are on their way out, so the new name reads as zeros — the same loss the
// concurrent hard-link race produces, reached through a plain serial order
// rather than an interleaving.
//
// POSIX link(2) reports ENOENT here, so the guard maps to ErrNotFound.
func TestCreateHardLink_RefusesUnlinkedTarget(t *testing.T) {
	t.Parallel()

	fx := newTestFixture(t)
	rootCtx := fx.rootContext()

	file, _, err := fx.service.CreateFile(rootCtx, fx.rootHandle, "victim.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	removed, _, err := fx.service.RemoveFile(rootCtx, fx.rootHandle, "victim.txt")
	if err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if removed.PayloadID == "" {
		t.Fatalf("RemoveFile did not report the content free after the last link; the premise of this test is gone")
	}

	count, err := fx.store.GetLinkCount(context.Background(), handle)
	if err != nil {
		t.Fatalf("GetLinkCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("link count after the last unlink = %d, want 0", count)
	}

	if _, err := fx.service.CreateHardLink(rootCtx, fx.rootHandle, "resurrected.txt", handle); err == nil {
		t.Fatalf("CreateHardLink to an unlinked inode succeeded; the new name resolves to content RemoveFile already reported free")
	} else if !metadata.IsNotFoundError(err) {
		t.Fatalf("CreateHardLink to an unlinked inode: got %v, want a not-found error", err)
	}
}
