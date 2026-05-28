package handlers

// Tests for SMB2 READ handler. These focus on the narrow scope:
// regular-file READ uses common.ReadFromBlockStore and hands Release to
// the encoder via SMBResponseBase.ReleaseData; pipe and symlink READ
// variants stay on their heap-allocated source buffers and MUST leave
// ReleaseData nil.
//
// Integration tests that exercise the full regular-file READ round trip
// require a full metadata+block-store fixture — those are covered by the
// whole-repo -race regression.

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ---------------------------------------------------------------------------
// handlePipeRead returns a *ReadResponse whose ReleaseData is nil. Pipe
// READ buffers are NOT pool-backed — pipe.ProcessRead returns a slice
// owned by PipeManager's state machine.
// ---------------------------------------------------------------------------

func TestRead_PipeRead_LeavesReleaseDataNil(t *testing.T) {
	h := NewHandler()
	h.PipeManager = rpc.NewPipeManager()

	ctx := NewSMBHandlerContext(context.TODO(), "test-client", 1, 1, 1)
	req := &ReadRequest{
		Length: 4096,
		Offset: 0,
	}
	openFile := &OpenFile{
		IsPipe:   true,
		PipeName: "srvsvc",
	}

	resp, err := h.handlePipeRead(ctx, req, openFile)
	if err != nil {
		t.Fatalf("handlePipeRead returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("handlePipeRead returned nil response")
	}
	// With no registered pipe for this FileID, the handler returns
	// StatusInvalidHandle — the important invariant under test is that
	// the returned *ReadResponse keeps ReleaseData nil on ALL pipe paths.
	if resp.ReleaseData != nil {
		t.Fatal("pipe read response must leave ReleaseData nil (pipes are NOT pool-backed)")
	}
	if resp.Status != types.StatusInvalidHandle {
		t.Logf("pipe read status = %v (expected StatusInvalidHandle without a registered pipe)", resp.Status)
	}
}

// ---------------------------------------------------------------------------
// handleSymlinkRead returns a *ReadResponse whose ReleaseData is nil.
// Symlink READ buffers are NOT pool-backed — mfsymlink.Encode returns a
// freshly heap-allocated slice, and copying it into a pooled buffer
// would be a pure memcpy with no benefit.
// ---------------------------------------------------------------------------

func TestRead_SymlinkRead_LeavesReleaseDataNil(t *testing.T) {
	h := NewHandler()

	ctx := NewSMBHandlerContext(context.TODO(), "test-client", 1, 1, 1)
	req := &ReadRequest{
		Length: 1067, // MFsymlink spec size
		Offset: 0,
	}
	openFile := &OpenFile{
		Path: "/test/link",
	}
	file := &metadata.File{
		Path: "/test/link",
		FileAttr: metadata.FileAttr{
			Type:       metadata.FileTypeSymlink,
			LinkTarget: "/target",
		},
	}

	resp, err := h.handleSymlinkRead(ctx, openFile, file, req)
	if err != nil {
		t.Fatalf("handleSymlinkRead returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("handleSymlinkRead returned nil response")
	}
	if resp.ReleaseData != nil {
		t.Fatal("symlink read response must leave ReleaseData nil (symlinks are NOT pool-backed)")
	}
	if resp.Status != types.StatusSuccess {
		t.Errorf("symlink read status = %v, want StatusSuccess", resp.Status)
	}
	if len(resp.Data) == 0 {
		t.Error("symlink read returned no data")
	}
}
