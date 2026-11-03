package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// An empty file has no content, so the operations that only act on content
// succeed on it without a content store, as they did before one was cut.
// Every regular file carries a PayloadID, so gating these on PayloadID alone
// would send an empty file to the no-content-store answer.
func TestEmptyFile_ContentOpsSucceedWithoutAContentStore(t *testing.T) {
	h, fileID, _, sessionID, treeID := setupSetInfoGateTest(t, gateFileWriteData|gateFileWriteAttr)
	ctx := gateContext(sessionID, treeID)

	flush, err := h.Flush(ctx, &FlushRequest{FileID: fileID})
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := flush.GetStatus(); got != types.StatusSuccess {
		t.Errorf("FLUSH of an empty file = 0x%08x, want STATUS_SUCCESS", uint32(got))
	}

	write, err := h.Write(ctx, &WriteRequest{FileID: fileID, Offset: 4096, DataOffset: 64 + 48})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := write.GetStatus(); got != types.StatusSuccess || write.Count != 0 {
		t.Errorf("zero-length WRITE = 0x%08x count %d, want STATUS_SUCCESS count 0", uint32(got), write.Count)
	}

	w := smbenc.NewWriter(16)
	w.WriteUint64(0)
	w.WriteUint64(4096)
	zero, err := h.handleSetZeroData(ctx, buildSparseIoctlRequest(FsctlSetZeroData, fileID, w.Bytes()))
	if err != nil {
		t.Fatalf("handleSetZeroData: %v", err)
	}
	if zero.Status != types.StatusSuccess {
		t.Errorf("SET_ZERO_DATA at EOF = 0x%08x, want STATUS_SUCCESS", uint32(zero.Status))
	}

	// A write that would store bytes is the one that needs a content store.
	data, err := h.Write(ctx, &WriteRequest{FileID: fileID, Length: 4, DataOffset: 64 + 48, Data: []byte("data")})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := data.GetStatus(); got == types.StatusSuccess {
		t.Error("a WRITE of bytes succeeded with no content store wired")
	}
}
