package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Read handles SMB2 READ command [MS-SMB2] 2.2.19, 2.2.20
func (h *Handler) Read(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 49 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.19
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 49
	// padding := body[2]
	// flags := body[3]
	length := binary.LittleEndian.Uint32(body[4:8])
	offset := binary.LittleEndian.Uint64(body[8:16])
	var fileID [16]byte
	copy(fileID[:], body[16:32])
	// minimumCount := binary.LittleEndian.Uint32(body[32:36])
	// channel := binary.LittleEndian.Uint32(body[36:40])
	// remainingBytes := binary.LittleEndian.Uint32(body[40:44])
	// readChannelInfoOffset := binary.LittleEndian.Uint16(body[44:46])
	// readChannelInfoLength := binary.LittleEndian.Uint16(body[46:48])

	// Validate file handle
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Check if it's a directory
	if openFile.IsDirectory {
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// Get mock file content
	mockFile := h.GetMockFile(openFile.ShareName, openFile.Path)
	if mockFile == nil {
		return NewErrorResult(types.StatusObjectNameNotFound), nil
	}

	// Handle directories
	if mockFile.IsDir {
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// Calculate read range
	contentLen := int64(len(mockFile.Content))
	if int64(offset) >= contentLen {
		return NewErrorResult(types.StatusEndOfFile), nil
	}

	readEnd := int64(offset) + int64(length)
	if readEnd > contentLen {
		readEnd = contentLen
	}

	data := mockFile.Content[offset:readEnd]

	// Build response [MS-SMB2] 2.2.20
	// Response structure size is 17, but we need to align the data
	// DataOffset is relative to start of SMB2 header (64 bytes)
	// We place data at offset 0x50 (80) from header = 16 bytes after response structure

	respHeaderSize := 16 // Fixed part of response
	dataOffset := 64 + respHeaderSize + 1 // Header + response fixed part + 1 byte padding

	resp := make([]byte, respHeaderSize+1+len(data))
	binary.LittleEndian.PutUint16(resp[0:2], 17)                     // StructureSize (17)
	resp[2] = byte(dataOffset)                                       // DataOffset
	resp[3] = 0                                                       // Reserved
	binary.LittleEndian.PutUint32(resp[4:8], uint32(len(data)))      // DataLength
	binary.LittleEndian.PutUint32(resp[8:12], 0)                     // DataRemaining
	binary.LittleEndian.PutUint32(resp[12:16], 0)                    // Reserved2
	// Padding byte at position 16
	resp[16] = 0
	// Data starts at position 17
	copy(resp[17:], data)

	return NewResult(types.StatusSuccess, resp), nil
}
