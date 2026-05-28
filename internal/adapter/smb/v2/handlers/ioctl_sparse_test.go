package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildSparseIoctlRequest assembles an SMB2 IOCTL request body with an
// optional input buffer. The InputOffset is reported relative to the SMB2
// header so the production parser (which lives at offset 56 of the body)
// resolves to the same bytes the test wrote.
func buildSparseIoctlRequest(ctlCode uint32, fileID [16]byte, input []byte) []byte {
	const fixedSize = 56
	w := smbenc.NewWriter(fixedSize + len(input))
	w.WriteUint16(57)                     // StructureSize
	w.WriteUint16(0)                      // Reserved
	w.WriteUint32(ctlCode)                // CtlCode
	w.WriteBytes(fileID[:])               // FileId
	w.WriteUint32(uint32(64 + fixedSize)) // InputOffset (header + fixed)
	w.WriteUint32(uint32(len(input)))     // InputCount
	w.WriteUint32(0)                      // MaxInputResponse
	w.WriteUint32(uint32(64 + fixedSize)) // OutputOffset
	w.WriteUint32(0)                      // OutputCount
	w.WriteUint32(0)                      // MaxOutputResponse
	w.WriteUint32(0)                      // Flags
	w.WriteUint32(0)                      // Reserved2
	if len(input) > 0 {
		w.WriteBytes(input)
	}
	return w.Bytes()
}

// TestSetSparse_NoHandle returns STATUS_FILE_CLOSED when the FileID has no
// matching open. This guards the dispatch-time gate added in MS-SMB2 3.3.5.15
// so smbtorture sparse tests get a clean error rather than a panic when they
// drive an unregistered handle.
func TestSetSparse_NoHandle(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{Context: context.Background()}

	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(i + 1)
	}
	body := buildSparseIoctlRequest(FsctlSetSparse, fileID, nil)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("Ioctl returned error: %v", err)
	}
	if result.Status != types.StatusFileClosed {
		t.Fatalf("status = 0x%08x, want STATUS_FILE_CLOSED", uint32(result.Status))
	}
}

// TestSetSparse_AccessDenied confirms the FILE_WRITE_DATA gate from MS-FSA
// §2.1.5.10.34. Handles opened with FILE_READ_DATA only must not be allowed
// to flip the sparse attribute (smb2.ioctl.sparse_perms exercises this).
func TestSetSparse_AccessDenied(t *testing.T) {
	h := NewHandler()
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(0x10 + i)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/sparse_perms",
		ShareName:     "share1",
		DesiredAccess: uint32(types.FileReadData),
		GrantedAccess: uint32(types.FileReadData),
	})
	ctx := &SMBHandlerContext{Context: context.Background()}

	body := buildSparseIoctlRequest(FsctlSetSparse, fileID, nil)
	result, err := h.handleSetSparse(ctx, body)
	if err != nil {
		t.Fatalf("handleSetSparse returned error: %v", err)
	}
	if result.Status != types.StatusAccessDenied {
		t.Fatalf("status = 0x%08x, want STATUS_ACCESS_DENIED", uint32(result.Status))
	}
}

// TestSetSparse_OversizeInput rejects FILE_SET_SPARSE_BUFFER payloads larger
// than 1 byte (Samba `fsctl_set_sparse` returns STATUS_INVALID_PARAMETER;
// covered by smb2.ioctl.sparse_set_oversize).
func TestSetSparse_OversizeInput(t *testing.T) {
	h := NewHandler()
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(0x20 + i)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/sparse_oversize",
		ShareName:     "share1",
		DesiredAccess: uint32(types.FileWriteData),
		GrantedAccess: uint32(types.FileWriteData),
	})
	ctx := &SMBHandlerContext{Context: context.Background()}

	body := buildSparseIoctlRequest(FsctlSetSparse, fileID, []byte{0x01, 0x02})
	result, err := h.handleSetSparse(ctx, body)
	if err != nil {
		t.Fatalf("handleSetSparse returned error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Fatalf("status = 0x%08x, want STATUS_INVALID_PARAMETER", uint32(result.Status))
	}
}

// TestSetSparse_Accepted exercises the success path: a handle with
// FILE_WRITE_DATA, an empty input buffer, and an open file id returns
// STATUS_SUCCESS plus an echo IOCTL response with the matching CtlCode.
// The test pins that DittoFS continues to advertise sparse-FSCTL support
// to smb2.set-sparse-ioctl.
func TestSetSparse_Accepted(t *testing.T) {
	h := NewHandler()
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(0x30 + i)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/sparse_ok",
		ShareName:     "share1",
		DesiredAccess: uint32(types.FileWriteData),
		GrantedAccess: uint32(types.FileWriteData),
	})
	ctx := &SMBHandlerContext{Context: context.Background()}

	body := buildSparseIoctlRequest(FsctlSetSparse, fileID, nil)
	result, err := h.handleSetSparse(ctx, body)
	if err != nil {
		t.Fatalf("handleSetSparse returned error: %v", err)
	}
	if result.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%08x, want STATUS_SUCCESS", uint32(result.Status))
	}
	if len(result.Data) < 48 {
		t.Fatalf("IOCTL response shorter than fixed header: %d bytes", len(result.Data))
	}
	gotCtl := smbenc.NewReader(result.Data[4:8]).ReadUint32()
	if gotCtl != FsctlSetSparse {
		t.Errorf("response CtlCode = 0x%08X, want 0x%08X", gotCtl, FsctlSetSparse)
	}
}

// TestQueryAllocatedRanges_MalformedInput rejects requests whose input is
// shorter than the 16-byte FILE_ALLOCATED_RANGE_BUFFER (smb2.ioctl.
// sparse_qar_malformed / sparse_qar_truncated).
func TestQueryAllocatedRanges_MalformedInput(t *testing.T) {
	h := NewHandler()
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(0x40 + i)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/qar_malformed",
		ShareName:     "share1",
		DesiredAccess: uint32(types.FileReadData),
		GrantedAccess: uint32(types.FileReadData),
	})
	ctx := &SMBHandlerContext{Context: context.Background()}

	// 8 bytes is half the required size.
	body := buildSparseIoctlRequest(FsctlQueryAllocatedRanges, fileID, make([]byte, 8))
	result, err := h.handleQueryAllocatedRanges(ctx, body)
	if err != nil {
		t.Fatalf("handleQueryAllocatedRanges returned error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Fatalf("status = 0x%08x, want STATUS_INVALID_PARAMETER", uint32(result.Status))
	}
}

// TestQueryAllocatedRanges_NegativeOffset rejects high-bit-set (parses-to-
// negative) FileOffset and Length. Matches Samba `fsctl_qar` precondition
// check (smb2.ioctl.sparse_qar_ob1).
func TestQueryAllocatedRanges_NegativeOffset(t *testing.T) {
	h := NewHandler()
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(0x50 + i)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/qar_ob1",
		ShareName:     "share1",
		DesiredAccess: uint32(types.FileReadData),
		GrantedAccess: uint32(types.FileReadData),
	})
	ctx := &SMBHandlerContext{Context: context.Background()}

	// Set high bit on FileOffset (negative when treated as int64).
	input := make([]byte, 16)
	for i := 0; i < 8; i++ {
		input[i] = 0xFF
	}
	body := buildSparseIoctlRequest(FsctlQueryAllocatedRanges, fileID, input)
	result, err := h.handleQueryAllocatedRanges(ctx, body)
	if err != nil {
		t.Fatalf("handleQueryAllocatedRanges returned error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Fatalf("status = 0x%08x, want STATUS_INVALID_PARAMETER", uint32(result.Status))
	}
}

// TestSetZeroData_AccessDenied verifies the write-access gate. Read-only
// handles must not be able to punch holes / zero data (MS-FSA §2.1.5.10.35).
func TestSetZeroData_AccessDenied(t *testing.T) {
	h := NewHandler()
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(0x60 + i)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/zero_perms",
		ShareName:     "share1",
		DesiredAccess: uint32(types.FileReadData),
		GrantedAccess: uint32(types.FileReadData),
	})
	ctx := &SMBHandlerContext{Context: context.Background()}

	input := make([]byte, 16) // 16 zero bytes — fileOffset = beyond = 0
	body := buildSparseIoctlRequest(FsctlSetZeroData, fileID, input)
	result, err := h.handleSetZeroData(ctx, body)
	if err != nil {
		t.Fatalf("handleSetZeroData returned error: %v", err)
	}
	if result.Status != types.StatusAccessDenied {
		t.Fatalf("status = 0x%08x, want STATUS_ACCESS_DENIED", uint32(result.Status))
	}
}

// TestSetZeroData_InverseRange rejects ranges where BeyondFinalZero <
// FileOffset (Samba `fsctl_zero_data` precondition).
func TestSetZeroData_InverseRange(t *testing.T) {
	h := NewHandler()
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(0x70 + i)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/zero_inverse",
		ShareName:     "share1",
		DesiredAccess: uint32(types.FileWriteData),
		GrantedAccess: uint32(types.FileWriteData),
	})
	ctx := &SMBHandlerContext{Context: context.Background()}

	// FileOffset = 100, BeyondFinalZero = 50.
	w := smbenc.NewWriter(16)
	w.WriteUint64(100)
	w.WriteUint64(50)
	body := buildSparseIoctlRequest(FsctlSetZeroData, fileID, w.Bytes())
	result, err := h.handleSetZeroData(ctx, body)
	if err != nil {
		t.Fatalf("handleSetZeroData returned error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Fatalf("status = 0x%08x, want STATUS_INVALID_PARAMETER", uint32(result.Status))
	}
}

// TestSetZeroData_ZeroLengthIsNoop is a documented no-op in Samba's
// fsctl_zero_data — FileOffset == BeyondFinalZero returns SUCCESS without
// touching the file. Skipping the write-path keeps the unit tests
// independent of the metadata/blockstore registry.
func TestSetZeroData_ZeroLengthIsNoop(t *testing.T) {
	h := NewHandler()
	var fileID [16]byte
	for i := range fileID {
		fileID[i] = byte(0x80 + i)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "/zero_noop",
		ShareName:     "share1",
		DesiredAccess: uint32(types.FileWriteData),
		GrantedAccess: uint32(types.FileWriteData),
	})
	ctx := &SMBHandlerContext{Context: context.Background()}

	w := smbenc.NewWriter(16)
	w.WriteUint64(1024)
	w.WriteUint64(1024)
	body := buildSparseIoctlRequest(FsctlSetZeroData, fileID, w.Bytes())
	result, err := h.handleSetZeroData(ctx, body)
	if err != nil {
		t.Fatalf("handleSetZeroData returned error: %v", err)
	}
	if result.Status != types.StatusSuccess {
		t.Fatalf("status = 0x%08x, want STATUS_SUCCESS", uint32(result.Status))
	}
}
