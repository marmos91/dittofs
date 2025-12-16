package handlers

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// QueryInfo handles SMB2 QUERY_INFO command [MS-SMB2] 2.2.37, 2.2.38
func (h *Handler) QueryInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeQueryInfoRequest(body)
	if err != nil {
		logger.Debug("QUERY_INFO: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("QUERY_INFO request",
		"infoType", req.InfoType,
		"fileInfoClass", req.FileInfoClass,
		"fileID", fmt.Sprintf("%x", req.FileID))

	// ========================================================================
	// Step 2: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("QUERY_INFO: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// ========================================================================
	// Step 3: Get metadata store and file attributes
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("QUERY_INFO: failed to get metadata store", "share", openFile.ShareName, "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	file, err := metadataStore.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("QUERY_INFO: failed to get file", "path", openFile.Path, "error", err)
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	// ========================================================================
	// Step 4: Build info based on type and class
	// ========================================================================

	var info []byte

	switch req.InfoType {
	case types.SMB2InfoTypeFile:
		info, err = h.buildFileInfoFromStore(file, types.FileInfoClass(req.FileInfoClass))
	case types.SMB2InfoTypeFilesystem:
		info, err = h.buildFilesystemInfo(ctx.Context, types.FileInfoClass(req.FileInfoClass), metadataStore, openFile.MetadataHandle)
	case types.SMB2InfoTypeSecurity:
		info, err = h.buildSecurityInfo()
	default:
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	if err != nil {
		return NewErrorResult(types.StatusNotSupported), nil
	}

	// Truncate if necessary
	if uint32(len(info)) > req.OutputBufferLength {
		info = info[:req.OutputBufferLength]
		return NewResult(types.StatusBufferOverflow, info), nil
	}

	// ========================================================================
	// Step 5: Build and encode response
	// ========================================================================

	resp := &QueryInfoResponse{
		Data: info,
	}

	respBytes, err := EncodeQueryInfoResponse(resp)
	if err != nil {
		logger.Warn("QUERY_INFO: failed to encode response", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	return NewResult(types.StatusSuccess, respBytes), nil
}

// buildFileInfoFromStore builds file information based on class using metadata store
func (h *Handler) buildFileInfoFromStore(file *metadata.File, class types.FileInfoClass) ([]byte, error) {
	switch class {
	case types.FileBasicInformation:
		basicInfo := FileAttrToFileBasicInfo(&file.FileAttr)
		return EncodeFileBasicInfo(basicInfo), nil

	case types.FileStandardInformation:
		standardInfo := FileAttrToFileStandardInfo(&file.FileAttr, false)
		return EncodeFileStandardInfo(standardInfo), nil

	case types.FileInternalInformation:
		// FILE_INTERNAL_INFORMATION [MS-FSCC] 2.4.20 (8 bytes)
		info := make([]byte, 8)
		// Convert UUID to uint64 by using first 8 bytes
		fileIndex := binary.LittleEndian.Uint64(file.ID[:8])
		binary.LittleEndian.PutUint64(info[0:8], fileIndex) // IndexNumber (unique file ID)
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
		networkInfo := FileAttrToFileNetworkOpenInfo(&file.FileAttr)
		return EncodeFileNetworkOpenInfo(networkInfo), nil

	case types.FileAllInformation:
		return h.buildFileAllInformationFromStore(file), nil

	default:
		return nil, types.ErrNotSupported
	}
}

// buildFileAllInformationFromStore builds FILE_ALL_INFORMATION from metadata
func (h *Handler) buildFileAllInformationFromStore(file *metadata.File) []byte {
	// FILE_ALL_INFORMATION [MS-FSCC] 2.4.2 (varies)
	// Basic (40) + Standard (24) + Internal (8) + EA (4) + Access (4) + Position (8) + Mode (4) + Alignment (4) + Name (variable)
	info := make([]byte, 100)

	basicInfo := FileAttrToFileBasicInfo(&file.FileAttr)
	standardInfo := FileAttrToFileStandardInfo(&file.FileAttr, false)

	// BasicInformation (40 bytes)
	basicBytes := EncodeFileBasicInfo(basicInfo)
	copy(info[0:40], basicBytes)

	// StandardInformation (24 bytes) starting at offset 40
	standardBytes := EncodeFileStandardInfo(standardInfo)
	copy(info[40:64], standardBytes)

	// InternalInformation (8 bytes) starting at offset 64
	fileIndex := binary.LittleEndian.Uint64(file.ID[:8])
	binary.LittleEndian.PutUint64(info[64:72], fileIndex)

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

	return info
}

// buildFilesystemInfo builds filesystem information [MS-FSCC] 2.5
func (h *Handler) buildFilesystemInfo(ctx context.Context, class types.FileInfoClass, metadataStore metadata.MetadataStore, handle metadata.FileHandle) ([]byte, error) {
	switch class {
	case 1: // FileFsVolumeInformation [MS-FSCC] 2.5.9
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
		// Try to get real filesystem stats
		blockSize := uint64(4096)
		stats, err := metadataStore.GetFilesystemStatistics(ctx, handle)
		if err == nil {
			totalBlocks := stats.TotalBytes / blockSize
			availBlocks := stats.AvailableBytes / blockSize
			info := make([]byte, 24)
			binary.LittleEndian.PutUint64(info[0:8], totalBlocks)
			binary.LittleEndian.PutUint64(info[8:16], availBlocks)
			binary.LittleEndian.PutUint32(info[16:20], 1)
			binary.LittleEndian.PutUint32(info[20:24], uint32(blockSize))
			return info, nil
		}
		// Fallback to hardcoded values
		info := make([]byte, 24)
		binary.LittleEndian.PutUint64(info[0:8], 1000000)
		binary.LittleEndian.PutUint64(info[8:16], 500000)
		binary.LittleEndian.PutUint32(info[16:20], 1)
		binary.LittleEndian.PutUint32(info[20:24], 4096)
		return info, nil

	case 5: // FileFsAttributeInformation [MS-FSCC] 2.5.1
		fsName := []byte{'N', 0, 'T', 0, 'F', 0, 'S', 0} // "NTFS" in UTF-16LE
		info := make([]byte, 12+len(fsName))
		binary.LittleEndian.PutUint32(info[0:4], 0x00000003) // FILE_CASE_SENSITIVE_SEARCH | FILE_CASE_PRESERVED_NAMES
		binary.LittleEndian.PutUint32(info[4:8], 255)
		binary.LittleEndian.PutUint32(info[8:12], uint32(len(fsName)))
		copy(info[12:], fsName)
		return info, nil

	case 6: // FileFsFullSizeInformation [MS-FSCC] 2.5.4
		blockSize := uint64(4096)
		stats, err := metadataStore.GetFilesystemStatistics(ctx, handle)
		if err == nil {
			totalBlocks := stats.TotalBytes / blockSize
			availBlocks := stats.AvailableBytes / blockSize
			info := make([]byte, 32)
			binary.LittleEndian.PutUint64(info[0:8], totalBlocks)
			binary.LittleEndian.PutUint64(info[8:16], availBlocks)
			binary.LittleEndian.PutUint64(info[16:24], availBlocks)
			binary.LittleEndian.PutUint32(info[24:28], 1)
			binary.LittleEndian.PutUint32(info[28:32], uint32(blockSize))
			return info, nil
		}
		// Fallback
		info := make([]byte, 32)
		binary.LittleEndian.PutUint64(info[0:8], 1000000)
		binary.LittleEndian.PutUint64(info[8:16], 500000)
		binary.LittleEndian.PutUint64(info[16:24], 500000)
		binary.LittleEndian.PutUint32(info[24:28], 1)
		binary.LittleEndian.PutUint32(info[28:32], 4096)
		return info, nil

	default:
		return nil, types.ErrNotSupported
	}
}

// buildSecurityInfo builds security information [MS-DTYP] 2.4.6
func (h *Handler) buildSecurityInfo() ([]byte, error) {
	// Return minimal security descriptor that grants everyone access
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
