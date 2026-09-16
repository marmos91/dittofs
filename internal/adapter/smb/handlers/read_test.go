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
	openFile := (&OpenFile{}).WithName(OpenName{Path: "/test/link"})
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

// ---------------------------------------------------------------------------
// Position semantics — MS-FSA 2.1.5.3 ("Server Requests a Read"): every successful READ advances
// Open.CurrentByteOffset to req.Offset + bytesReturned. The smb2.read.position
// torture test reads that value back via GetInfo FilePositionInformation.
// recordReadProgress() centralises the update so every success path —
// regular file, zero-length read, symlink — applies the same rule.
// ---------------------------------------------------------------------------

func TestRecordReadProgress_AdvancesPositionInfo(t *testing.T) {
	tests := []struct {
		name          string
		startPos      uint64
		offset        uint64
		bytesReturned uint64
		want          uint64
	}{
		{"regular read advances past offset", 0, 100, 50, 150},
		{"zero-length read pins position to offset", 0, 200, 0, 200},
		{"successive read accumulates", 150, 150, 50, 200},
		{"overwrites previous position", 999, 0, 10, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			of := &OpenFile{PositionInfo: tt.startPos}
			recordReadProgress(of, tt.offset, tt.bytesReturned)
			if of.PositionInfo != tt.want {
				t.Errorf("PositionInfo = %d, want %d", of.PositionInfo, tt.want)
			}
		})
	}
}

func TestRecordReadProgress_NilOpenFileIsSafe(t *testing.T) {
	// Defensive — handlers always pass a non-nil OpenFile, but the helper
	// must not panic if a future caller forgets.
	recordReadProgress(nil, 100, 50)
}

// TestHandleSymlinkRead_AdvancesPositionInfo locks in the comment-1 fix:
// the symlink success path must also call recordReadProgress, otherwise
// smb2.read.position would observe a stale CurrentByteOffset on symlinks.
func TestHandleSymlinkRead_AdvancesPositionInfo(t *testing.T) {
	h := NewHandler()

	ctx := NewSMBHandlerContext(context.TODO(), "test-client", 1, 1, 1)
	req := &ReadRequest{
		Length: 100,
		Offset: 10,
	}
	openFile := (&OpenFile{}).WithName(OpenName{Path: "/test/link"})
	file := &metadata.File{
		Path: "/test/link",
		FileAttr: metadata.FileAttr{
			Type:       metadata.FileTypeSymlink,
			LinkTarget: "/target",
		},
	}

	resp, err := h.handleSymlinkRead(ctx, openFile, file, req)
	if err != nil {
		t.Fatalf("handleSymlinkRead: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("symlink read status = %v, want StatusSuccess", resp.Status)
	}
	want := req.Offset + uint64(len(resp.Data))
	if openFile.PositionInfo != want {
		t.Errorf("PositionInfo = %d, want %d (offset+bytesReturned)", openFile.PositionInfo, want)
	}
}

// ---------------------------------------------------------------------------
// READ access gate — hasReadAccess accepts FILE_EXECUTE in addition to
// FILE_READ_DATA. Samba and Windows allow READ on a handle opened with only
// FILE_EXECUTE (execution implies read), and the smb2.read.access torture
// test exercises that path.
// ---------------------------------------------------------------------------

func TestHasReadAccess_AcceptedMasks(t *testing.T) {
	tests := []struct {
		name   string
		access uint32
		want   bool
	}{
		{"FILE_READ_DATA grants read", uint32(types.FileReadData), true},
		{"FILE_EXECUTE grants read (execution implies read)", uint32(types.FileExecute), true},
		{"GENERIC_READ grants read", uint32(types.GenericRead), true},
		{"GENERIC_ALL grants read", uint32(types.GenericAll), true},
		{"MAXIMUM_ALLOWED grants read", uint32(types.MaximumAllowed), true},
		{"FILE_WRITE_DATA alone does not grant read", uint32(types.FileWriteData), false},
		{"DELETE alone does not grant read", uint32(types.Delete), false},
		{"empty mask does not grant read", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasReadAccess(tt.access); got != tt.want {
				t.Errorf("hasReadAccess(0x%x) = %v, want %v", tt.access, got, tt.want)
			}
		})
	}
}

// TestReadWrite_RdmaChannel_PipeAlsoRefused covers the half of the fail-closed
// RDMA rule that the file path's own test cannot reach.
//
// READ and WRITE dispatch to the pipe handlers before running their validation
// block, so a gate placed after that dispatch is applied to regular files only
// and a named pipe is served inline with the RDMA semantics the client asked
// for silently dropped. The transport is the same one either way, so the pipe
// can honor a channel no better than a file can.
//
// Without the gate ahead of the dispatch these return STATUS_INVALID_HANDLE
// from the pipe handler (no pipe is registered for the FileID), which is a
// different refusal for a different reason — so the assertion discriminates.
func TestReadWrite_RdmaChannel_PipeAlsoRefused(t *testing.T) {
	newPipeHandler := func(t *testing.T) (*Handler, [16]byte) {
		t.Helper()
		h := NewHandler()
		h.PipeManager = rpc.NewPipeManager()
		fileID := [16]byte{0xF1, 0xF2}
		h.StoreOpenFile(&OpenFile{FileID: fileID, IsPipe: true, PipeName: "srvsvc"})
		return h, fileID
	}

	t.Run("READ", func(t *testing.T) {
		h, fileID := newPipeHandler(t)
		resp, err := h.Read(NewSMBHandlerContext(context.TODO(), "test-client", 1, 1, 1), &ReadRequest{
			FileID:  fileID,
			Length:  4,
			Channel: 1, // SMB2_CHANNEL_RDMA_V1
		})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got := resp.GetStatus(); got != types.StatusInvalidParameter {
			t.Fatalf("pipe READ with RDMA channel: status = 0x%08x, want STATUS_INVALID_PARAMETER (0x%08x)",
				uint32(got), uint32(types.StatusInvalidParameter))
		}
	})

	t.Run("WRITE", func(t *testing.T) {
		h, fileID := newPipeHandler(t)
		resp, err := h.Write(NewSMBHandlerContext(context.TODO(), "test-client", 1, 1, 1), &WriteRequest{
			FileID:  fileID,
			Data:    []byte("x"),
			Channel: 1,
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got := resp.GetStatus(); got != types.StatusInvalidParameter {
			t.Fatalf("pipe WRITE with RDMA channel: status = 0x%08x, want STATUS_INVALID_PARAMETER (0x%08x)",
				uint32(got), uint32(types.StatusInvalidParameter))
		}
	})
}
