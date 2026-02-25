package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// =============================================================================
// Test Helper Functions
// =============================================================================

// buildCreateRequestBody builds a minimal CREATE request body for testing.
// The body follows [MS-SMB2] 2.2.13 format (56 bytes fixed + filename).
func buildCreateRequestBody(filename string, disposition types.CreateDisposition, options types.CreateOptions) []byte {
	// Encode filename as UTF-16LE
	nameBytes := encodeUTF16LE(filename)

	// Fixed size: 56 bytes + filename
	body := make([]byte, 56+len(nameBytes))

	// StructureSize at offset 0 (always 57)
	binary.LittleEndian.PutUint16(body[0:2], 57)

	// SecurityFlags at offset 2
	body[2] = 0

	// RequestedOplockLevel at offset 3
	body[3] = 0 // No oplock

	// ImpersonationLevel at offset 4
	binary.LittleEndian.PutUint32(body[4:8], 0)

	// SmbCreateFlags at offset 8 (reserved, 8 bytes)
	// Reserved at offset 16 (8 bytes)

	// DesiredAccess at offset 24
	binary.LittleEndian.PutUint32(body[24:28], 0x12019F) // Generic read/write

	// FileAttributes at offset 28
	binary.LittleEndian.PutUint32(body[28:32], uint32(types.FileAttributeNormal))

	// ShareAccess at offset 32
	binary.LittleEndian.PutUint32(body[32:36], 0x07) // Read + Write + Delete

	// CreateDisposition at offset 36
	binary.LittleEndian.PutUint32(body[36:40], uint32(disposition))

	// CreateOptions at offset 40
	binary.LittleEndian.PutUint32(body[40:44], uint32(options))

	// NameOffset at offset 44 (64 header + 56 fixed = 120)
	binary.LittleEndian.PutUint16(body[44:46], 120)

	// NameLength at offset 46
	binary.LittleEndian.PutUint16(body[46:48], uint16(len(nameBytes)))

	// CreateContextsOffset at offset 48
	binary.LittleEndian.PutUint32(body[48:52], 0)

	// CreateContextsLength at offset 52
	binary.LittleEndian.PutUint32(body[52:56], 0)

	// Filename at offset 56
	if len(nameBytes) > 0 {
		copy(body[56:], nameBytes)
	}

	return body
}

// =============================================================================
// DecodeCreateRequest Tests
// =============================================================================

func TestDecodeCreateRequest_ShortBody(t *testing.T) {
	t.Run("BodyShorterThan56Bytes", func(t *testing.T) {
		shortBody := make([]byte, 30)

		_, err := DecodeCreateRequest(shortBody)
		if err == nil {
			t.Error("Expected error for body shorter than 56 bytes")
		}
	})

	t.Run("EmptyBody", func(t *testing.T) {
		_, err := DecodeCreateRequest([]byte{})
		if err == nil {
			t.Error("Expected error for empty body")
		}
	})

	t.Run("NilBody", func(t *testing.T) {
		_, err := DecodeCreateRequest(nil)
		if err == nil {
			t.Error("Expected error for nil body")
		}
	})
}

func TestDecodeCreateRequest_MinimumValidBody(t *testing.T) {
	// Exactly 56 bytes with no filename
	body := make([]byte, 56)
	binary.LittleEndian.PutUint16(body[0:2], 57) // StructureSize

	req, err := DecodeCreateRequest(body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if req.FileName != "" {
		t.Errorf("FileName should be empty, got %q", req.FileName)
	}
}

func TestDecodeCreateRequest_ValidRequest(t *testing.T) {
	body := buildCreateRequestBody("test.txt", types.FileOpenIf, 0)

	req, err := DecodeCreateRequest(body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if req.FileName != "test.txt" {
		t.Errorf("FileName = %q, expected %q", req.FileName, "test.txt")
	}

	if req.CreateDisposition != types.FileOpenIf {
		t.Errorf("CreateDisposition = %d, expected %d (FileOpenIf)",
			req.CreateDisposition, types.FileOpenIf)
	}

	if req.DesiredAccess != 0x12019F {
		t.Errorf("DesiredAccess = 0x%x, expected 0x12019F", req.DesiredAccess)
	}
}

func TestDecodeCreateRequest_ParsesFields(t *testing.T) {
	t.Run("OplockLevel", func(t *testing.T) {
		body := make([]byte, 56)
		binary.LittleEndian.PutUint16(body[0:2], 57)
		body[3] = 0x08 // Batch oplock

		req, err := DecodeCreateRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if req.OplockLevel != 0x08 {
			t.Errorf("OplockLevel = 0x%x, expected 0x08", req.OplockLevel)
		}
	})

	t.Run("ImpersonationLevel", func(t *testing.T) {
		body := make([]byte, 56)
		binary.LittleEndian.PutUint16(body[0:2], 57)
		binary.LittleEndian.PutUint32(body[4:8], 2) // Impersonation

		req, err := DecodeCreateRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if req.ImpersonationLevel != 2 {
			t.Errorf("ImpersonationLevel = %d, expected 2", req.ImpersonationLevel)
		}
	})

	t.Run("ShareAccess", func(t *testing.T) {
		body := make([]byte, 56)
		binary.LittleEndian.PutUint16(body[0:2], 57)
		binary.LittleEndian.PutUint32(body[32:36], 0x03) // Read + Write

		req, err := DecodeCreateRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if req.ShareAccess != 0x03 {
			t.Errorf("ShareAccess = 0x%x, expected 0x03", req.ShareAccess)
		}
	})

	t.Run("CreateOptions", func(t *testing.T) {
		body := make([]byte, 56)
		binary.LittleEndian.PutUint16(body[0:2], 57)
		binary.LittleEndian.PutUint32(body[40:44], uint32(types.FileDirectoryFile))

		req, err := DecodeCreateRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if req.CreateOptions != types.FileDirectoryFile {
			t.Errorf("CreateOptions = 0x%x, expected 0x%x (FileDirectoryFile)",
				req.CreateOptions, types.FileDirectoryFile)
		}
	})

	t.Run("FileAttributes", func(t *testing.T) {
		body := make([]byte, 56)
		binary.LittleEndian.PutUint16(body[0:2], 57)
		binary.LittleEndian.PutUint32(body[28:32], uint32(types.FileAttributeDirectory))

		req, err := DecodeCreateRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if req.FileAttributes != types.FileAttributeDirectory {
			t.Errorf("FileAttributes = 0x%x, expected 0x%x (FileAttributeDirectory)",
				req.FileAttributes, types.FileAttributeDirectory)
		}
	})
}

func TestDecodeCreateRequest_ZeroNameLength(t *testing.T) {
	body := make([]byte, 56)
	binary.LittleEndian.PutUint16(body[0:2], 57) // StructureSize
	// NameOffset and NameLength are both 0

	req, err := DecodeCreateRequest(body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if req.FileName != "" {
		t.Errorf("FileName should be empty for zero name length, got %q", req.FileName)
	}
}

func TestDecodeCreateRequest_PathWithBackslashes(t *testing.T) {
	body := buildCreateRequestBody("subdir\\file.txt", types.FileOpen, types.FileNonDirectoryFile)

	req, err := DecodeCreateRequest(body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// The decoder should preserve backslashes as-is (conversion happens in handler)
	if req.FileName != "subdir\\file.txt" {
		t.Errorf("FileName = %q, expected %q", req.FileName, "subdir\\file.txt")
	}
}

// =============================================================================
// CreateResponse Encode Tests
// =============================================================================

func TestCreateResponse_Encode(t *testing.T) {
	t.Run("EncodesCorrectLength", func(t *testing.T) {
		resp := &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			CreateAction:    types.FileCreated,
		}

		data, err := resp.Encode()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// Response should be 89 bytes per MS-SMB2 2.2.14
		if len(data) != 89 {
			t.Errorf("Response length = %d, expected 89", len(data))
		}
	})

	t.Run("EncodesStructureSize", func(t *testing.T) {
		resp := &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		}

		data, err := resp.Encode()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		structSize := binary.LittleEndian.Uint16(data[0:2])
		if structSize != 89 {
			t.Errorf("StructureSize = %d, expected 89", structSize)
		}
	})

	t.Run("EncodesCreateAction", func(t *testing.T) {
		resp := &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			CreateAction:    types.FileCreated,
		}

		data, err := resp.Encode()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		action := types.CreateAction(binary.LittleEndian.Uint32(data[4:8]))
		if action != types.FileCreated {
			t.Errorf("CreateAction = %d, expected %d (FileCreated)",
				action, types.FileCreated)
		}
	})

	t.Run("EncodesFileID", func(t *testing.T) {
		var fileID [16]byte
		fileID[0] = 0xAB
		fileID[15] = 0xCD

		resp := &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			FileID:          fileID,
		}

		data, err := resp.Encode()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// FileID at offset 64-80
		if data[64] != 0xAB {
			t.Errorf("FileID[0] = 0x%x, expected 0xAB", data[64])
		}
		if data[79] != 0xCD {
			t.Errorf("FileID[15] = 0x%x, expected 0xCD", data[79])
		}
	})

	t.Run("EncodesFileSize", func(t *testing.T) {
		resp := &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			AllocationSize:  8192,
			EndOfFile:       5000,
		}

		data, err := resp.Encode()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		allocSize := binary.LittleEndian.Uint64(data[40:48])
		if allocSize != 8192 {
			t.Errorf("AllocationSize = %d, expected 8192", allocSize)
		}

		endOfFile := binary.LittleEndian.Uint64(data[48:56])
		if endOfFile != 5000 {
			t.Errorf("EndOfFile = %d, expected 5000", endOfFile)
		}
	})

	t.Run("EncodesFileAttributes", func(t *testing.T) {
		resp := &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			FileAttributes:  types.FileAttributeDirectory,
		}

		data, err := resp.Encode()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		attrs := types.FileAttributes(binary.LittleEndian.Uint32(data[56:60]))
		if attrs != types.FileAttributeDirectory {
			t.Errorf("FileAttributes = 0x%x, expected 0x%x (Directory)",
				attrs, types.FileAttributeDirectory)
		}
	})

	t.Run("EncodesOplockLevel", func(t *testing.T) {
		resp := &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			OplockLevel:     0x08, // Batch
		}

		data, err := resp.Encode()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if data[2] != 0x08 {
			t.Errorf("OplockLevel = 0x%x, expected 0x08", data[2])
		}
	})
}
