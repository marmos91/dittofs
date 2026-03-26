package handlers

import (
	"crypto/md5"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleGetCompression handles FSCTL_GET_COMPRESSION [MS-FSCC] 2.3.9.
// Returns the compression format for the open file. DittoFS does not actually
// compress data, but tracks the compression state persistently in metadata.
// The compression format is derived from the modeDOSCompressed mode bit.
func (h *Handler) handleGetCompression(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// Read compression state from metadata (persistent across handles).
	var format uint16
	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(ctx.Context, openFile.MetadataHandle)
	if err == nil && file.Mode&modeDOSCompressed != 0 {
		format = 0x0002 // COMPRESSION_FORMAT_LZNT1
	}

	logger.Debug("IOCTL FSCTL_GET_COMPRESSION", "format", format)

	w := smbenc.NewWriter(2)
	w.WriteUint16(format)
	resp := buildIoctlResponse(FsctlGetCompression, fileID, w.Bytes())
	return NewResult(types.StatusSuccess, resp), nil
}

// handleSetCompression handles FSCTL_SET_COMPRESSION [MS-FSCC] 2.3.53.
// Validates the compression format and persists the state to metadata.
// DittoFS does not actually compress data but maintains protocol-correct state,
// including FILE_ATTRIBUTE_COMPRESSED in file attributes.
// Per MS-FSCC 2.3.53: valid values are 0 (NONE), 1 (DEFAULT), 2 (LZNT1).
func (h *Handler) handleSetCompression(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// Parse InputBuffer: CompressionState (2 bytes) at InputOffset
	inputData := parseIoctlInputData(body)
	if len(inputData) < 2 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	format := uint16(inputData[0]) | uint16(inputData[1])<<8
	logger.Debug("IOCTL FSCTL_SET_COMPRESSION", "format", format)

	// Per MS-FSCC 2.3.53: valid formats are 0 (NONE), 1 (DEFAULT), 2 (LZNT1).
	// DEFAULT and LZNT1 both map to LZNT1 (the only compression algorithm NTFS supports).
	var storedFormat uint32
	switch format {
	case 0x0000: // COMPRESSION_FORMAT_NONE
		storedFormat = 0
	case 0x0001, 0x0002: // COMPRESSION_FORMAT_DEFAULT, COMPRESSION_FORMAT_LZNT1
		storedFormat = 0x0002
	default:
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Update per-handle state
	openFile.CompressionFormat.Store(storedFormat)

	// Persist compression state to metadata via mode bit (modeDOSCompressed).
	// This ensures FILE_ATTRIBUTE_COMPRESSED survives handle close/reopen.
	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	newMode := file.Mode
	if storedFormat != 0 {
		newMode |= modeDOSCompressed
	} else {
		newMode &^= modeDOSCompressed
	}

	if newMode != file.Mode {
		authCtx, authErr := BuildAuthContext(ctx)
		if authErr != nil {
			logger.Warn("FSCTL_SET_COMPRESSION: failed to build auth context", "error", authErr)
		} else if err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, &metadata.SetAttrs{
			Mode: &newMode,
		}); err != nil {
			logger.Warn("FSCTL_SET_COMPRESSION: failed to persist mode", "error", err)
			// Non-fatal: per-handle state is still set
		}
	}

	resp := buildIoctlResponse(FsctlSetCompression, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
}

// handleGetIntegrityInfo handles FSCTL_GET_INTEGRITY_INFORMATION [MS-FSCC] 2.3.25.
// Per MS-FSA 2.1.5.9.15: if the object store does not implement this functionality,
// the operation MUST be failed with STATUS_INVALID_DEVICE_REQUEST.
func (h *Handler) handleGetIntegrityInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("IOCTL FSCTL_GET_INTEGRITY_INFORMATION: not supported (INVALID_DEVICE_REQUEST)")
	return NewErrorResult(types.StatusInvalidDeviceRequest), nil
}

// handleSetIntegrityInfo handles FSCTL_SET_INTEGRITY_INFORMATION [MS-FSCC] 2.3.55.
// Per MS-FSA 2.1.5.9.29: if the object store does not implement this functionality,
// the operation MUST be failed with STATUS_INVALID_DEVICE_REQUEST.
func (h *Handler) handleSetIntegrityInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("IOCTL FSCTL_SET_INTEGRITY_INFORMATION: not supported (INVALID_DEVICE_REQUEST)")
	return NewErrorResult(types.StatusInvalidDeviceRequest), nil
}

// handleGetObjectID handles FSCTL_GET_OBJECT_ID [MS-FSCC] 2.3.28.
// Returns a deterministic object ID derived from the file handle.
// Per MS-FSA 2.1.5.9.17: return the object ID for the file.
func (h *Handler) handleGetObjectID(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	return h.buildObjectIDResponse(FsctlGetObjectID, body)
}

// handleCreateOrGetObjectID handles FSCTL_CREATE_OR_GET_OBJECT_ID [MS-FSCC] 2.3.7.
// Returns the same object ID as FSCTL_GET_OBJECT_ID (creates if not present).
func (h *Handler) handleCreateOrGetObjectID(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	return h.buildObjectIDResponse(FsctlCreateOrGetObjectID, body)
}

// buildObjectIDResponse builds a FILE_OBJECTID_BUFFER response [MS-FSCC] 2.1.3.
// Shared by FSCTL_GET_OBJECT_ID and FSCTL_CREATE_OR_GET_OBJECT_ID since DittoFS
// always derives the object ID deterministically from the metadata handle.
func (h *Handler) buildObjectIDResponse(ctlCode uint32, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// Per MS-FSCC: OutputBufferSize must be >= sizeof(FILE_OBJECTID_BUFFER) = 64
	maxOutputSize := parseIoctlMaxOutputSize(body)
	if maxOutputSize < 64 {
		logger.Debug("IOCTL object ID: output buffer too small", "maxOutput", maxOutputSize)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("IOCTL object ID", "ctlCode", ctlCode, "path", openFile.Path)

	// FILE_OBJECTID_BUFFER: ObjectId(16) + BirthVolumeId(16) + BirthObjectId(16) + DomainId(16)
	output := make([]byte, 64)
	objID := deriveObjectID(openFile.MetadataHandle)
	copy(output[0:16], objID[:])
	copy(output[16:32], h.ServerGUID[:]) // BirthVolumeId
	copy(output[32:48], objID[:])        // BirthObjectId = ObjectId
	// DomainId: zeros (default)

	resp := buildIoctlResponse(ctlCode, fileID, output)
	return NewResult(types.StatusSuccess, resp), nil
}

// handleMarkHandle handles FSCTL_MARK_HANDLE [MS-FSCC] 2.3.36.
// Per MS-FSA 2.1.5.9.20: for directory streams, fail with STATUS_DIRECTORY_NOT_SUPPORTED.
// For data streams, return STATUS_SUCCESS.
func (h *Handler) handleMarkHandle(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusFileClosed), nil
	}

	logger.Debug("IOCTL FSCTL_MARK_HANDLE", "path", openFile.Path, "isDir", openFile.IsDirectory)

	// Per MS-FSA 2.1.5.9.20: if StreamType == DirectoryStream, fail
	if openFile.IsDirectory {
		return NewErrorResult(statusDirectoryNotSupported), nil
	}

	resp := buildIoctlResponse(FsctlMarkHandle, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
}

// handleQueryFileRegions handles FSCTL_QUERY_FILE_REGIONS [MS-FSCC] 2.3.51.
// Returns a single region covering the entire file (all data is allocated).
func (h *Handler) handleQueryFileRegions(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusFileClosed), nil
	}

	logger.Debug("IOCTL FSCTL_QUERY_FILE_REGIONS", "path", openFile.Path)

	// Get file size from metadata
	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}
	size := getSMBSize(&file.FileAttr)

	// FILE_REGION_OUTPUT [MS-FSCC] 2.3.51:
	// Flags(4) + TotalRegionEntryCount(4) + RegionEntryCount(4) + Reserved(4)
	// + FILE_REGION_INFO[]: FileOffset(8) + Length(8) + Usage(4) + Reserved(4)
	var w *smbenc.Writer
	if size == 0 {
		// Empty file: no regions to report
		w = smbenc.NewWriter(16)
		w.WriteUint32(0) // Flags
		w.WriteUint32(0) // TotalRegionEntryCount
		w.WriteUint32(0) // RegionEntryCount
		w.WriteUint32(0) // Reserved
	} else {
		w = smbenc.NewWriter(16 + 24)
		w.WriteUint32(0)    // Flags
		w.WriteUint32(1)    // TotalRegionEntryCount
		w.WriteUint32(1)    // RegionEntryCount
		w.WriteUint32(0)    // Reserved
		w.WriteUint64(0)    // FileOffset
		w.WriteUint64(size) // Length
		w.WriteUint32(1)    // Usage: FILE_REGION_USAGE_VALID_NONCACHED_DATA
		w.WriteUint32(0)    // Reserved
	}

	resp := buildIoctlResponse(FsctlQueryFileRegions, fileID, w.Bytes())
	return NewResult(types.StatusSuccess, resp), nil
}

// deriveObjectID creates a deterministic 16-byte object ID from a metadata handle
// by computing its MD5 hash.
func deriveObjectID(handle []byte) [16]byte {
	return md5.Sum(handle)
}

// parseIoctlMaxOutputSize extracts MaxOutputResponse from an IOCTL request body.
// IOCTL request layout: StructureSize(2) + Reserved(2) + CtlCode(4) + FileId(16)
// + InputOffset(4) + InputCount(4) + MaxInputResponse(4) + OutputOffset(4)
// + OutputCount(4) + MaxOutputResponse(4) = offset 44, 4 bytes.
func parseIoctlMaxOutputSize(body []byte) uint32 {
	if len(body) < 48 {
		return 0
	}
	r := smbenc.NewReader(body[44:])
	return r.ReadUint32()
}

// parseIoctlInputData extracts the input buffer from an IOCTL request body.
// IOCTL request layout: StructureSize(2) + Reserved(2) + CtlCode(4) + FileId(16)
// + InputOffset(4) + InputCount(4) ...
// InputOffset is relative to the start of the SMB2 header (64 bytes before body).
func parseIoctlInputData(body []byte) []byte {
	if len(body) < 32 {
		return nil
	}
	r := smbenc.NewReader(body[24:])
	inputOffset := r.ReadUint32()
	inputCount := r.ReadUint32()
	if inputCount == 0 {
		return nil
	}
	// InputOffset is relative to SMB2 header start; body starts 64 bytes after header
	bodyOffset := int(inputOffset) - 64
	if bodyOffset < 0 || bodyOffset+int(inputCount) > len(body) {
		return nil
	}
	return body[bodyOffset : bodyOffset+int(inputCount)]
}

// statusDirectoryNotSupported is STATUS_DIRECTORY_NOT_SUPPORTED (0xC00000FB).
// Per MS-ERREF: The request is not supported for directory streams.
const statusDirectoryNotSupported types.Status = 0xC00000FB
