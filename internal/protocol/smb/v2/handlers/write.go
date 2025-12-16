package handlers

import (
	"encoding/binary"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Write handles SMB2 WRITE command [MS-SMB2] 2.2.21, 2.2.22
func (h *Handler) Write(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 49 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.21
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 49
	dataOffset := binary.LittleEndian.Uint16(body[2:4])
	length := binary.LittleEndian.Uint32(body[4:8])
	offset := binary.LittleEndian.Uint64(body[8:16])
	var fileID [16]byte
	copy(fileID[:], body[16:32])
	// channel := binary.LittleEndian.Uint32(body[32:36])
	// remainingBytes := binary.LittleEndian.Uint32(body[36:40])
	// writeChannelInfoOffset := binary.LittleEndian.Uint16(body[40:42])
	// writeChannelInfoLength := binary.LittleEndian.Uint16(body[42:44])
	// flags := binary.LittleEndian.Uint32(body[44:48])

	// Validate file handle
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Check if it's a directory
	if openFile.IsDirectory {
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// Get mock file
	mockFile := h.GetMockFile(openFile.ShareName, openFile.Path)
	if mockFile == nil {
		// For Phase 1, create a temporary mock file for writes
		mockFile = &MockFile{
			Name:       openFile.Path,
			IsDir:      false,
			Size:       0,
			Content:    nil,
			Created:    time.Now(),
			Modified:   time.Now(),
			Accessed:   time.Now(),
			Attributes: uint32(types.FileAttributeNormal),
		}
	}

	// Handle directories
	if mockFile.IsDir {
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// Extract write data
	// dataOffset is relative to SMB2 header start (64 bytes)
	adjustedOffset := int(dataOffset) - 64

	var writeData []byte
	if adjustedOffset >= 0 && adjustedOffset+int(length) <= len(body) {
		writeData = body[adjustedOffset : adjustedOffset+int(length)]
	} else if int(length) <= len(body)-48 {
		// Data might be right after the fixed structure
		writeData = body[48 : 48+int(length)]
	} else {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// For Phase 1 with mock data, we simulate writes
	// In a real implementation, this would write to the content store
	newSize := int64(offset) + int64(len(writeData))
	if newSize > mockFile.Size {
		// Expand content buffer if needed
		if int(newSize) > len(mockFile.Content) {
			newContent := make([]byte, newSize)
			copy(newContent, mockFile.Content)
			mockFile.Content = newContent
		}
		mockFile.Size = newSize
	}
	copy(mockFile.Content[offset:], writeData)
	mockFile.Modified = time.Now()

	// Build response [MS-SMB2] 2.2.22 (17 bytes)
	resp := make([]byte, 17)
	binary.LittleEndian.PutUint16(resp[0:2], 17)                     // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], 0)                      // Reserved
	binary.LittleEndian.PutUint32(resp[4:8], uint32(len(writeData))) // Count (bytes written)
	binary.LittleEndian.PutUint32(resp[8:12], 0)                     // Remaining
	binary.LittleEndian.PutUint16(resp[12:14], 0)                    // WriteChannelInfoOffset
	binary.LittleEndian.PutUint16(resp[14:16], 0)                    // WriteChannelInfoLength

	return NewResult(types.StatusSuccess, resp), nil
}
