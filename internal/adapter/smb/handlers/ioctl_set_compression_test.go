package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildSetCompressionRequest builds a full 56-byte SMB2 IOCTL envelope plus an
// optional InputBuffer carrying the 2-byte CompressionFormat. The InputOffset
// uses the standard 64-byte SMB2 header convention (header + 56-byte fixed
// envelope = 120). Pass inputData=nil to omit the InputBuffer entirely (used
// to drive the post-gate INVALID_PARAMETER short-input branch).
func buildSetCompressionRequest(fileID [16]byte, inputData []byte) []byte {
	const fixed = 56
	w := smbenc.NewWriter(fixed + len(inputData))
	w.WriteUint16(57) // StructureSize
	w.WriteUint16(0)  // Reserved
	w.WriteUint32(FsctlSetCompression)
	w.WriteBytes(fileID[:])
	if len(inputData) > 0 {
		w.WriteUint32(64 + fixed) // InputOffset (header is 64 bytes before body, then 56-byte envelope)
		w.WriteUint32(uint32(len(inputData)))
	} else {
		w.WriteUint32(0) // InputOffset
		w.WriteUint32(0) // InputCount
	}
	w.WriteUint32(0) // MaxInputResponse
	w.WriteUint32(0) // OutputOffset
	w.WriteUint32(0) // OutputCount
	w.WriteUint32(0) // MaxOutputResponse
	w.WriteUint32(0) // Flags
	w.WriteUint32(0) // Reserved2
	if len(inputData) > 0 {
		w.WriteBytes(inputData)
	}
	return w.Bytes()
}

// TestHandleSetCompression_AccessGate_DeniesWithoutWriteData pins the
// FILE_WRITE_DATA access gate that smbtorture smb2.ioctl.compress_perms
// regression-tests. The test opens a handle whose GrantedAccess records
// FILE_APPEND_DATA | FILE_WRITE_ATTRIBUTES (matching smbtorture's
// `SEC_RIGHTS_FILE_WRITE & ~SEC_FILE_WRITE_DATA`) and asserts the FSCTL
// returns STATUS_ACCESS_DENIED — APPEND_DATA and WRITE_ATTRIBUTES alone
// must NOT satisfy Samba's `check_any_access_fsp(fsp, FILE_WRITE_DATA)`.
func TestHandleSetCompression_AccessGate_DeniesWithoutWriteData(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	fileID := [16]byte{0x01, 0x02, 0x03, 0x04}
	h.StoreOpenFile(&OpenFile{
		FileID: fileID,
		Path:   "compress_perms_no_write_data",
		// FILE_APPEND_DATA | FILE_WRITE_ATTRIBUTES — the smbtorture-shaped
		// mask that strips FILE_WRITE_DATA from SEC_RIGHTS_FILE_WRITE.
		GrantedAccess: uint32(types.FileAppendData) | uint32(types.FileWriteAttributes),
	})

	// CompressionFormat=COMPRESSION_FORMAT_DEFAULT (matches one of the two
	// formats smbtorture submits before asserting ACCESS_DENIED).
	body := buildSetCompressionRequest(fileID, []byte{0x01, 0x00})

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusAccessDenied {
		t.Errorf("expected StatusAccessDenied for FILE_APPEND_DATA|FILE_WRITE_ATTRIBUTES handle, got %v",
			result.Status)
	}
}

// TestHandleSetCompression_AccessGate_DeniesReadOnly pins the read-only
// handle path: a handle granted FILE_READ_DATA alone must be rejected by
// the FILE_WRITE_DATA gate. This is the obvious case but is worth pinning
// separately so the gate cannot regress to e.g. `(WRITE_DATA|READ_DATA)`
// non-zero checks.
func TestHandleSetCompression_AccessGate_DeniesReadOnly(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	fileID := [16]byte{0x05, 0x06, 0x07, 0x08}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "compress_perms_read_only",
		GrantedAccess: uint32(types.FileReadData) | uint32(types.FileReadAttributes),
	})

	body := buildSetCompressionRequest(fileID, []byte{0x00, 0x00}) // COMPRESSION_FORMAT_NONE

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusAccessDenied {
		t.Errorf("expected StatusAccessDenied for read-only handle, got %v", result.Status)
	}
}

// TestHandleSetCompression_AccessGate_PassesWithWriteData pins the positive
// case: when GrantedAccess includes FILE_WRITE_DATA the access gate must
// NOT short-circuit to STATUS_ACCESS_DENIED. We assert this by sending a
// zero-length InputBuffer; that fails downstream with
// STATUS_INVALID_PARAMETER from `parseIoctlInputData`, which runs only
// after the gate. STATUS_INVALID_PARAMETER (not STATUS_ACCESS_DENIED)
// proves the gate accepted the handle. Going further (STATUS_SUCCESS)
// would require wiring a full metadata-service registry, which is out of
// scope for this unit test.
func TestHandleSetCompression_AccessGate_PassesWithWriteData(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	fileID := [16]byte{0x09, 0x0A, 0x0B, 0x0C}
	h.StoreOpenFile(&OpenFile{
		FileID:        fileID,
		Path:          "compress_perms_with_write_data",
		GrantedAccess: uint32(types.FileWriteData),
	})

	// Zero-length InputBuffer — gate must pass first, then
	// parseIoctlInputData rejects on len < 2.
	body := buildSetCompressionRequest(fileID, nil)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status == types.StatusAccessDenied {
		t.Fatalf("FILE_WRITE_DATA handle must clear the access gate; got STATUS_ACCESS_DENIED")
	}
	if result.Status != types.StatusInvalidParameter {
		t.Errorf("expected StatusInvalidParameter from post-gate short-input branch, got %v",
			result.Status)
	}
}
