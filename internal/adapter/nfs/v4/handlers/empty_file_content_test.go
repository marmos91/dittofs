package handlers

import (
	"bytes"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/cubbit/dittofs/internal/adapter/nfs/xdr/core"
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

// TestReadPlusAndSeek_AnswerLikeRead: a file whose bytes are in no chunk row
// (they may sit in a journal) answers READ_PLUS and SEEK_DATA with the I/O
// error READ gives, never a hole that reads its data back as zeros.
func TestReadPlusAndSeek_AnswerLikeRead(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	f := fx.createRegularFile(t, fx.rootHandle, "f.txt", 0o644, 0, 0)
	fx.writeContent(t, f, []byte("data"))
	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, f)

	var buf bytes.Buffer
	writeStateid(&buf, anonStateid())
	_ = xdr.WriteUint64(&buf, 0)
	_ = xdr.WriteUint32(&buf, 4096)
	if res := fx.handler.handleReadPlus(ctx, &buf); res.Status != types.NFS4ERR_IO {
		t.Errorf("READ_PLUS = %d, want NFS4ERR_IO", res.Status)
	}
	if res := fx.handler.handleSeek(ctx, encSeekArgs(anonStateid(), 0, types.NFS4_CONTENT_DATA)); res.Status != types.NFS4ERR_IO {
		t.Errorf("SEEK_DATA = %d, want NFS4ERR_IO", res.Status)
	}
}
