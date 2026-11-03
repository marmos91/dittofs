package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// TestDeallocate_EmptyFile succeeds without a content store: there is no
// content in an empty file to punch, though every regular file carries a
// PayloadID. A range inside a file with a size is the one that needs a
// content store.
func TestDeallocate_EmptyFile(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	empty := fx.createRegularFile(t, fx.rootHandle, "empty.txt", 0o644, 0, 0)
	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, empty)
	if res := fx.handler.handleDeallocate(ctx, encAllocArgs(anonStateid(), 0, 4096)); res.Status != types.NFS4_OK {
		t.Errorf("DEALLOCATE of an empty file = %d, want NFS4_OK", res.Status)
	}

	full := fx.createRegularFile(t, fx.rootHandle, "full.txt", 0o644, 0, 0)
	fx.writeContent(t, full, []byte("data"))
	setCurrentFH(ctx, full)
	if res := fx.handler.handleDeallocate(ctx, encAllocArgs(anonStateid(), 0, 4096)); res.Status != types.NFS4ERR_IO {
		t.Errorf("DEALLOCATE inside a file with content = %d, want NFS4ERR_IO", res.Status)
	}
}
