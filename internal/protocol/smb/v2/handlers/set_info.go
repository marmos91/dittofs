package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// SetInfo handles SMB2 SET_INFO command [MS-SMB2] 2.2.39, 2.2.40
func (h *Handler) SetInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 33 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.39
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 33
	infoType := body[2]
	fileInfoClass := body[3]
	bufferLength := binary.LittleEndian.Uint32(body[4:8])
	bufferOffset := binary.LittleEndian.Uint16(body[8:10])
	// reserved := binary.LittleEndian.Uint16(body[10:12])
	// additionalInformation := binary.LittleEndian.Uint32(body[12:16])
	var fileID [16]byte
	copy(fileID[:], body[16:32])

	// Validate file handle
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Get mock file
	mockFile := h.GetMockFile(openFile.ShareName, openFile.Path)
	if mockFile == nil {
		return NewErrorResult(types.StatusObjectNameNotFound), nil
	}

	// Extract buffer data
	adjustedOffset := int(bufferOffset) - 64 // Relative to SMB2 header
	if adjustedOffset < 0 || adjustedOffset+int(bufferLength) > len(body) {
		// Try offset from start of structure
		adjustedOffset = 32
	}

	var buffer []byte
	if adjustedOffset >= 0 && adjustedOffset+int(bufferLength) <= len(body) {
		buffer = body[adjustedOffset : adjustedOffset+int(bufferLength)]
	}

	switch infoType {
	case types.SMB2InfoTypeFile:
		return h.setFileInfo(mockFile, fileInfoClass, buffer)
	case types.SMB2InfoTypeSecurity:
		// For Phase 1, accept but ignore security updates
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil
	default:
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
}

// setFileInfo handles setting file information
func (h *Handler) setFileInfo(f *MockFile, class uint8, buffer []byte) (*HandlerResult, error) {
	switch class {
	case types.FileBasicInformation:
		// FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7 (40 bytes)
		if len(buffer) < 36 {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}
		// Parse times (we don't actually update mock data, but validate format)
		// creationTime := binary.LittleEndian.Uint64(buffer[0:8])
		// lastAccessTime := binary.LittleEndian.Uint64(buffer[8:16])
		// lastWriteTime := binary.LittleEndian.Uint64(buffer[16:24])
		// changeTime := binary.LittleEndian.Uint64(buffer[24:32])
		attrs := binary.LittleEndian.Uint32(buffer[32:36])
		if attrs != 0 {
			f.Attributes = attrs
		}
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	case types.FileRenameInformation:
		// FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34
		// For Phase 1, accept but ignore
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	case 13: // FileDispositionInformation [MS-FSCC] 2.4.11
		// For Phase 1, accept but ignore delete requests
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	case 20: // FileEndOfFileInformation [MS-FSCC] 2.4.13
		// Set end of file (truncate/extend)
		if len(buffer) < 8 {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}
		newSize := binary.LittleEndian.Uint64(buffer[0:8])
		f.Size = int64(newSize)
		// Adjust content if needed
		if int(newSize) < len(f.Content) {
			f.Content = f.Content[:newSize]
		} else if int(newSize) > len(f.Content) {
			newContent := make([]byte, newSize)
			copy(newContent, f.Content)
			f.Content = newContent
		}
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	case 19: // FileAllocationInformation [MS-FSCC] 2.4.4
		// Set allocation size - for Phase 1, treat like EOF
		if len(buffer) < 8 {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}
		// Accept but don't actually change allocation
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	default:
		return NewErrorResult(types.StatusNotSupported), nil
	}
}
