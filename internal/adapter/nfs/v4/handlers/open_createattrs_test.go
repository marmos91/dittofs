package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestOpen_Unchecked4CreateAppliesCreateAttrs covers the first half of
// RFC 7530 §16.16.3: on a create that actually creates, "createattrs specifies
// the initial set of attributes for the file". Only the mode reached
// CreateFile, so a create carrying a size produced an empty file instead.
func TestOpen_Unchecked4CreateAppliesCreateAttrs(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	clientID := testClientID(t, fx.handler.StateManager, "createattrs-client")
	owner := []byte("createattrs-owner")

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	args := encodeOpenUncheckedSizeArgs(1, types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE, clientID, owner, 32, "sized.txt")

	if r := fx.handler.handleOpen(ctx, bytes.NewReader(args)); r.Status != types.NFS4_OK {
		t.Fatalf("create with size=32: status = %d, want NFS4_OK", r.Status)
	}

	file, err := fx.metaSvc.GetFile(context.Background(), metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		t.Fatalf("get created file: %v", err)
	}
	if file.Size != 32 {
		t.Fatalf("size of a file created with size=32: got %d, want 32", file.Size)
	}
}

// TestOpen_Unchecked4ExistingIgnoresNonZeroSize covers the second half:
// "When an UNCHECKED4 create encounters an existing file, the attributes
// specified by createattrs are not used, except that when a size of zero is
// specified, the existing file is truncated." The whole set used to be applied,
// so a plain reopen resized the file to whatever length the client named.
func TestOpen_Unchecked4ExistingIgnoresNonZeroSize(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	clientID := testClientID(t, fx.handler.StateManager, "resize-client")
	owner := []byte("resize-owner")

	fh := fx.createRegularFile(t, fx.rootHandle, "resize.txt", 0o644, 0, 0)
	fx.writeContent(t, fh, []byte("hello world contents"))

	pre, err := fx.metaSvc.GetFile(context.Background(), fh)
	if err != nil {
		t.Fatalf("get file pre: %v", err)
	}
	if pre.Size == 0 {
		t.Fatalf("setup: expected non-zero size before the reopen")
	}

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fx.rootHandle)
	args := encodeOpenUncheckedSizeArgs(1, types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE, clientID, owner, 4, "resize.txt")

	if r := fx.handler.handleOpen(ctx, bytes.NewReader(args)); r.Status != types.NFS4_OK {
		t.Fatalf("reopen with size=4: status = %d, want NFS4_OK", r.Status)
	}

	post, err := fx.metaSvc.GetFile(context.Background(), fh)
	if err != nil {
		t.Fatalf("get file post: %v", err)
	}
	if post.Size != pre.Size {
		t.Fatalf("size after a reopen naming size=4: got %d, want it left at %d", post.Size, pre.Size)
	}
}
