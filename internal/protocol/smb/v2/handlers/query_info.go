package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// QueryInfo handles SMB2 QUERY_INFO command [MS-SMB2] 2.2.37, 2.2.38
func (h *Handler) QueryInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("QUERY_INFO request", "bodyLen", len(body))

	// StructureSize is 41 but actual fixed fields are 40 bytes (FileID ends at offset 40)
	// In compound requests, the body may be exactly 40 bytes
	if len(body) < 40 {
		logger.Debug("QUERY_INFO body too small", "len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.37
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 41
	infoType := body[2]
	fileInfoClass := types.FileInfoClass(body[3])
	outputBufferLength := binary.LittleEndian.Uint32(body[4:8])
	// inputBufferOffset := binary.LittleEndian.Uint16(body[8:10])
	// reserved := binary.LittleEndian.Uint16(body[10:12])
	// inputBufferLength := binary.LittleEndian.Uint32(body[12:16])
	// additionalInformation := binary.LittleEndian.Uint32(body[16:20])
	// flags := binary.LittleEndian.Uint32(body[20:24])
	var fileID [16]byte
	copy(fileID[:], body[24:40])

	logger.Debug("QUERY_INFO parsed",
		"infoType", infoType,
		"fileInfoClass", fileInfoClass,
		"outputBufferLength", outputBufferLength,
		"fileID", fmt.Sprintf("%x", fileID))

	// Validate file handle
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("QUERY_INFO invalid handle", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}
	logger.Debug("QUERY_INFO found open file", "path", openFile.Path, "shareName", openFile.ShareName)

	// Get mock file
	mockFile := h.GetMockFile(openFile.ShareName, openFile.Path)
	if mockFile == nil {
		// Root directory case
		mockFile = &MockFile{
			Name:       "",
			IsDir:      true,
			Attributes: uint32(types.FileAttributeDirectory),
		}
	}

	var info []byte
	var err error

	switch infoType {
	case types.SMB2InfoTypeFile:
		info, err = h.buildFileInfo(mockFile, fileInfoClass)
	case types.SMB2InfoTypeFilesystem:
		info, err = h.buildFilesystemInfo(fileInfoClass)
	case types.SMB2InfoTypeSecurity:
		info, err = h.buildSecurityInfo()
	default:
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	if err != nil {
		return NewErrorResult(types.StatusNotSupported), nil
	}

	// Truncate if necessary
	if uint32(len(info)) > outputBufferLength {
		info = info[:outputBufferLength]
		// Return buffer overflow if data was truncated
		return NewResult(types.StatusBufferOverflow, info), nil
	}

	// Build response [MS-SMB2] 2.2.38
	resp := make([]byte, 8+len(info))
	binary.LittleEndian.PutUint16(resp[0:2], 9)                 // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], 72)                // OutputBufferOffset (64 + 8)
	binary.LittleEndian.PutUint32(resp[4:8], uint32(len(info))) // OutputBufferLength
	copy(resp[8:], info)

	return NewResult(types.StatusSuccess, resp), nil
}

// buildFileInfo builds file information based on class [MS-FSCC] 2.4
func (h *Handler) buildFileInfo(f *MockFile, class types.FileInfoClass) ([]byte, error) {
	now := types.NowFiletime()

	switch class {
	case types.FileBasicInformation:
		// FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7 (40 bytes)
		info := make([]byte, 40)
		binary.LittleEndian.PutUint64(info[0:8], now)            // CreationTime
		binary.LittleEndian.PutUint64(info[8:16], now)           // LastAccessTime
		binary.LittleEndian.PutUint64(info[16:24], now)          // LastWriteTime
		binary.LittleEndian.PutUint64(info[24:32], now)          // ChangeTime
		binary.LittleEndian.PutUint32(info[32:36], f.Attributes) // FileAttributes
		binary.LittleEndian.PutUint32(info[36:40], 0)            // Reserved
		return info, nil

	case types.FileStandardInformation:
		// FILE_STANDARD_INFORMATION [MS-FSCC] 2.4.41 (24 bytes)
		info := make([]byte, 24)
		binary.LittleEndian.PutUint64(info[0:8], uint64(f.Size))  // AllocationSize
		binary.LittleEndian.PutUint64(info[8:16], uint64(f.Size)) // EndOfFile
		binary.LittleEndian.PutUint32(info[16:20], 1)             // NumberOfLinks
		if f.IsDir {
			info[20] = 0 // DeletePending
			info[21] = 1 // Directory
		} else {
			info[20] = 0 // DeletePending
			info[21] = 0 // Directory
		}
		binary.LittleEndian.PutUint16(info[22:24], 0) // Reserved
		return info, nil

	case types.FileInternalInformation:
		// FILE_INTERNAL_INFORMATION [MS-FSCC] 2.4.20 (8 bytes)
		info := make([]byte, 8)
		binary.LittleEndian.PutUint64(info[0:8], 1) // IndexNumber (unique file ID)
		return info, nil

	case types.FileEaInformation:
		// FILE_EA_INFORMATION [MS-FSCC] 2.4.12 (4 bytes)
		info := make([]byte, 4)
		binary.LittleEndian.PutUint32(info[0:4], 0) // EaSize
		return info, nil

	case types.FileAccessInformation:
		// FILE_ACCESS_INFORMATION [MS-FSCC] 2.4.1 (4 bytes)
		info := make([]byte, 4)
		binary.LittleEndian.PutUint32(info[0:4], 0x001F01FF) // AccessFlags (full access)
		return info, nil

	case types.FileNetworkOpenInformation:
		// FILE_NETWORK_OPEN_INFORMATION [MS-FSCC] 2.4.28 (56 bytes)
		info := make([]byte, 56)
		binary.LittleEndian.PutUint64(info[0:8], now)
		binary.LittleEndian.PutUint64(info[8:16], now)
		binary.LittleEndian.PutUint64(info[16:24], now)
		binary.LittleEndian.PutUint64(info[24:32], now)
		binary.LittleEndian.PutUint64(info[32:40], uint64(f.Size))
		binary.LittleEndian.PutUint64(info[40:48], uint64(f.Size))
		binary.LittleEndian.PutUint32(info[48:52], f.Attributes)
		binary.LittleEndian.PutUint32(info[52:56], 0) // Reserved
		return info, nil

	case types.FileAllInformation:
		// FILE_ALL_INFORMATION [MS-FSCC] 2.4.2 (varies)
		// Basic (40) + Standard (24) + Internal (8) + EA (4) + Access (4) + Position (8) + Mode (4) + Alignment (4) + Name (variable)
		// For simplicity, return basic info only
		info := make([]byte, 100)
		// BasicInformation (40 bytes)
		binary.LittleEndian.PutUint64(info[0:8], now)
		binary.LittleEndian.PutUint64(info[8:16], now)
		binary.LittleEndian.PutUint64(info[16:24], now)
		binary.LittleEndian.PutUint64(info[24:32], now)
		binary.LittleEndian.PutUint32(info[32:36], f.Attributes)
		// StandardInformation (24 bytes) starting at offset 40
		binary.LittleEndian.PutUint64(info[40:48], uint64(f.Size))
		binary.LittleEndian.PutUint64(info[48:56], uint64(f.Size))
		binary.LittleEndian.PutUint32(info[56:60], 1)
		if f.IsDir {
			info[60] = 0
			info[61] = 1
		}
		// InternalInformation (8 bytes) starting at offset 64
		binary.LittleEndian.PutUint64(info[64:72], 1)
		// EaInformation (4 bytes) starting at offset 72
		binary.LittleEndian.PutUint32(info[72:76], 0)
		// AccessInformation (4 bytes) starting at offset 76
		binary.LittleEndian.PutUint32(info[76:80], 0x001F01FF)
		// PositionInformation (8 bytes) starting at offset 80
		binary.LittleEndian.PutUint64(info[80:88], 0)
		// ModeInformation (4 bytes) starting at offset 88
		binary.LittleEndian.PutUint32(info[88:92], 0)
		// AlignmentInformation (4 bytes) starting at offset 92
		binary.LittleEndian.PutUint32(info[92:96], 0)
		// NameInformation (4 bytes for length) starting at offset 96
		binary.LittleEndian.PutUint32(info[96:100], 0)
		return info, nil

	default:
		return nil, types.ErrNotSupported
	}
}

// buildFilesystemInfo builds filesystem information [MS-FSCC] 2.5
func (h *Handler) buildFilesystemInfo(class types.FileInfoClass) ([]byte, error) {
	switch class {
	case 1: // FileFsVolumeInformation [MS-FSCC] 2.5.9
		// VolumeCreationTime (8) + VolumeSerialNumber (4) + VolumeLabelLength (4) +
		// SupportsObjects (1) + Reserved (1) + VolumeLabel (variable)
		label := []byte{'D', 0, 'i', 0, 't', 0, 't', 0, 'o', 0, 'F', 0, 'S', 0} // "DittoFS" in UTF-16LE
		info := make([]byte, 18+len(label))
		binary.LittleEndian.PutUint64(info[0:8], types.NowFiletime())
		binary.LittleEndian.PutUint32(info[8:12], 0x12345678) // VolumeSerialNumber
		binary.LittleEndian.PutUint32(info[12:16], uint32(len(label)))
		info[16] = 0 // SupportsObjects
		info[17] = 0 // Reserved
		copy(info[18:], label)
		return info, nil

	case 3: // FileFsSizeInformation [MS-FSCC] 2.5.8
		info := make([]byte, 24)
		binary.LittleEndian.PutUint64(info[0:8], 1000000) // TotalAllocationUnits
		binary.LittleEndian.PutUint64(info[8:16], 500000) // AvailableAllocationUnits
		binary.LittleEndian.PutUint32(info[16:20], 1)     // SectorsPerAllocationUnit
		binary.LittleEndian.PutUint32(info[20:24], 4096)  // BytesPerSector
		return info, nil

	case 5: // FileFsAttributeInformation [MS-FSCC] 2.5.1
		// FileSystemAttributes (4) + MaxComponentNameLength (4) + FileSystemNameLength (4) + FileSystemName
		fsName := []byte{'N', 0, 'T', 0, 'F', 0, 'S', 0} // "NTFS" in UTF-16LE
		info := make([]byte, 12+len(fsName))
		binary.LittleEndian.PutUint32(info[0:4], 0x00000003) // FILE_CASE_SENSITIVE_SEARCH | FILE_CASE_PRESERVED_NAMES
		binary.LittleEndian.PutUint32(info[4:8], 255)        // MaxComponentNameLength
		binary.LittleEndian.PutUint32(info[8:12], uint32(len(fsName)))
		copy(info[12:], fsName)
		return info, nil

	case 6: // FileFsFullSizeInformation [MS-FSCC] 2.5.4
		info := make([]byte, 32)
		binary.LittleEndian.PutUint64(info[0:8], 1000000)  // TotalAllocationUnits
		binary.LittleEndian.PutUint64(info[8:16], 500000)  // CallerAvailableAllocationUnits
		binary.LittleEndian.PutUint64(info[16:24], 500000) // ActualAvailableAllocationUnits
		binary.LittleEndian.PutUint32(info[24:28], 1)      // SectorsPerAllocationUnit
		binary.LittleEndian.PutUint32(info[28:32], 4096)   // BytesPerSector
		return info, nil

	default:
		return nil, types.ErrNotSupported
	}
}

// buildSecurityInfo builds security information [MS-DTYP] 2.4.6
func (h *Handler) buildSecurityInfo() ([]byte, error) {
	// Return minimal security descriptor
	// For Phase 1, return a basic descriptor that grants everyone access
	// This is a minimal self-relative security descriptor
	info := make([]byte, 20)
	info[0] = 1                                      // Revision
	info[1] = 0                                      // Sbz1
	binary.LittleEndian.PutUint16(info[2:4], 0x8004) // Control (SE_SELF_RELATIVE | SE_DACL_PRESENT)
	binary.LittleEndian.PutUint32(info[4:8], 0)      // OffsetOwner
	binary.LittleEndian.PutUint32(info[8:12], 0)     // OffsetGroup
	binary.LittleEndian.PutUint32(info[12:16], 0)    // OffsetSacl
	binary.LittleEndian.PutUint32(info[16:20], 0)    // OffsetDacl

	return info, nil
}
