package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// allFFFileID is the sentinel FileID (all 0xFF bytes) required by
// FSCTL_VALIDATE_NEGOTIATE_INFO per [MS-SMB2] 2.2.31.4.
var allFFFileID = bytes.Repeat([]byte{0xFF}, 16)

// Common IOCTL/FSCTL codes [MS-FSCC] 2.3
const (
	FsctlDfsGetReferrals        uint32 = 0x00060194 // [MS-FSCC] 2.3.16
	FsctlPipeWait               uint32 = 0x00110018 // [MS-FSCC] 2.3.49
	FsctlPipeTransceive         uint32 = 0x0011C017 // [MS-FSCC] 2.3.50 - Named pipe transact
	FsctlValidateNegotiateInfo  uint32 = 0x00140204 // [MS-SMB2] 2.2.31.4
	FsctlQueryNetworkInterfInfo uint32 = 0x001401FC // [MS-SMB2] 2.2.32.5
	FsctlPipePeek               uint32 = 0x0011400C // [MS-FSCC] 2.3.48
	FsctlSrvEnumerateSnapshots  uint32 = 0x00144064 // [MS-SMB2] 2.2.32.2
	FsctlSrvRequestResumeKey    uint32 = 0x00140078 // [MS-SMB2] 2.2.32.3
	FsctlSrvCopyChunk           uint32 = 0x001440F2 // [MS-SMB2] 2.2.32.1
	FsctlSrvCopyChunkWrite      uint32 = 0x001480F2 // [MS-SMB2] 2.2.32.1
	FsctlGetReparsePoint        uint32 = 0x000900A8 // [MS-FSCC] 2.3.30
	FsctlGetNtfsVolumeData      uint32 = 0x00090064 // [MS-FSCC] 2.3.29 - NTFS volume data
	FsctlReadFileUsnData        uint32 = 0x000900EB // [MS-FSCC] 2.3.56 - Read file USN data
)

// Reparse point constants [MS-FSCC] 2.1.2.1
const (
	IoReparseTagSymlink uint32 = 0xA000000C
)

// Ioctl handles the SMB2 IOCTL command [MS-SMB2] 2.2.31, 2.2.32.
// It dispatches filesystem control codes including FSCTL_VALIDATE_NEGOTIATE_INFO
// (man-in-the-middle protection), FSCTL_GET_REPARSE_POINT (symlink target reads),
// FSCTL_PIPE_TRANSCEIVE (named pipe RPC), and FSCTL_SRV_ENUMERATE_SNAPSHOTS.
// Unsupported FSCTLs return StatusNotSupported gracefully.
func (h *Handler) Ioctl(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// Minimum size to read CtlCode is 8 bytes (StructureSize + Reserved + CtlCode)
	if len(body) < 8 {
		logger.Debug("IOCTL request too small", "len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	ctlCode := binary.LittleEndian.Uint32(body[4:8])
	logger.Debug("IOCTL request",
		"ctlCode", fmt.Sprintf("0x%08X", ctlCode),
		"bodyLen", len(body))

	// Handle specific IOCTLs
	switch ctlCode {
	case FsctlValidateNegotiateInfo:
		// Validate negotiation parameters [MS-SMB2] 2.2.31.4
		// This prevents man-in-the-middle attacks that could downgrade the connection
		return h.handleValidateNegotiateInfo(ctx, body)

	case FsctlQueryNetworkInterfInfo:
		// Query network interfaces - not critical, return NOT_SUPPORTED
		logger.Debug("IOCTL FSCTL_QUERY_NETWORK_INTERFACE_INFO - not supported")
		return NewErrorResult(types.StatusNotSupported), nil

	case FsctlDfsGetReferrals:
		// DFS referrals - we don't support DFS
		logger.Debug("IOCTL FSCTL_DFS_GET_REFERRALS - not supported")
		return NewErrorResult(types.StatusNotSupported), nil

	case FsctlGetReparsePoint:
		// FSCTL_GET_REPARSE_POINT - read symlink target [MS-FSCC] 2.3.30
		return h.handleGetReparsePoint(ctx, body)

	case FsctlSrvEnumerateSnapshots:
		// FSCTL_SRV_ENUMERATE_SNAPSHOTS [MS-SMB2] 2.2.32.2
		// Return empty snapshot list so Windows "Previous Versions" tab shows
		// "no previous versions" instead of an error.
		logger.Debug("IOCTL FSCTL_SRV_ENUMERATE_SNAPSHOTS: returning empty snapshot list")
		if len(body) < 24 {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}
		var fileID [16]byte
		copy(fileID[:], body[8:24])
		// SRV_SNAPSHOT_ARRAY: NumberOfSnapshots(4) + NumberOfSnapshotsReturned(4) + SnapshotArraySize(4)
		output := make([]byte, 12)
		resp := buildIoctlResponse(FsctlSrvEnumerateSnapshots, fileID, output)
		return NewResult(types.StatusSuccess, resp), nil

	case FsctlPipeTransceive:
		// FSCTL_PIPE_TRANSCEIVE - named pipe transact [MS-FSCC] 2.3.50
		// Combined write+read for RPC over named pipes
		return h.handlePipeTransceive(ctx, body)

	case FsctlGetNtfsVolumeData:
		// FSCTL_GET_NTFS_VOLUME_DATA [MS-FSCC] 2.3.29
		// Returns NTFS_VOLUME_DATA_BUFFER. Required by WPTS FileIdInformation
		// tests to verify VolumeSerialNumber matches FILE_ID_INFORMATION.
		return h.handleGetNtfsVolumeData(ctx, body)

	case FsctlReadFileUsnData:
		// FSCTL_READ_FILE_USN_DATA [MS-FSCC] 2.3.56
		// Returns a USN_RECORD_V2 for the file. Required by WPTS FSA tests
		// as a prerequisite for FileIdInformation and FilePositionInformation queries.
		return h.handleReadFileUsnData(ctx, body)

	default:
		logger.Debug("IOCTL unknown control code - not supported",
			"ctlCode", fmt.Sprintf("0x%08X", ctlCode))
		return NewErrorResult(types.StatusNotSupported), nil
	}
}

// handleGetReparsePoint handles FSCTL_GET_REPARSE_POINT for readlink
func (h *Handler) handleGetReparsePoint(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// IOCTL request structure [MS-SMB2] 2.2.31:
	// - StructureSize (2 bytes)
	// - Reserved (2 bytes)
	// - CtlCode (4 bytes)
	// - FileId (16 bytes at offset 8)
	if len(body) < 24 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	var fileID [16]byte
	copy(fileID[:], body[8:24])

	// Get open file
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL GET_REPARSE_POINT: invalid file ID", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Build auth context
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("IOCTL GET_REPARSE_POINT: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// Read symlink target
	metaSvc := h.Registry.GetMetadataService()
	target, _, err := metaSvc.ReadSymlink(authCtx, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("IOCTL GET_REPARSE_POINT: not a symlink or read failed",
			"path", openFile.Path, "error", err)
		// Check if it's not a symlink
		if storeErr, ok := err.(*metadata.StoreError); ok && storeErr.Code == metadata.ErrInvalidArgument {
			return NewErrorResult(types.StatusNotAReparsePoint), nil
		}
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	logger.Debug("IOCTL GET_REPARSE_POINT: symlink target", "path", openFile.Path, "target", target)

	// Build SYMBOLIC_LINK_REPARSE_DATA_BUFFER [MS-FSCC] 2.1.2.4
	reparseData := buildSymlinkReparseBuffer(target)

	// Build IOCTL response [MS-SMB2] 2.2.32
	resp := buildIoctlResponse(FsctlGetReparsePoint, fileID, reparseData)

	return NewResult(types.StatusSuccess, resp), nil
}

// buildSymlinkReparseBuffer builds SYMBOLIC_LINK_REPARSE_DATA_BUFFER [MS-FSCC] 2.1.2.4
func buildSymlinkReparseBuffer(target string) []byte {
	// Convert target to UTF-16LE
	targetUTF16 := utf16.Encode([]rune(target))
	targetBytes := make([]byte, len(targetUTF16)*2)
	for i, r := range targetUTF16 {
		binary.LittleEndian.PutUint16(targetBytes[i*2:], r)
	}

	// SYMBOLIC_LINK_REPARSE_DATA_BUFFER structure:
	// - ReparseTag (4 bytes) - IO_REPARSE_TAG_SYMLINK
	// - ReparseDataLength (2 bytes) - length of data after this field
	// - Reserved (2 bytes)
	// - SubstituteNameOffset (2 bytes)
	// - SubstituteNameLength (2 bytes)
	// - PrintNameOffset (2 bytes)
	// - PrintNameLength (2 bytes)
	// - Flags (4 bytes) - 0 = absolute, 1 = relative
	// - PathBuffer (variable) - contains both names

	// We put the same path in both SubstituteName and PrintName
	pathBufferLen := len(targetBytes) * 2 // Both names
	reparseDataLen := 12 + pathBufferLen  // 12 bytes for offsets/lengths/flags + paths

	buf := make([]byte, 8+reparseDataLen) // 8 byte header + reparse data

	// Header
	binary.LittleEndian.PutUint32(buf[0:4], IoReparseTagSymlink)    // ReparseTag
	binary.LittleEndian.PutUint16(buf[4:6], uint16(reparseDataLen)) // ReparseDataLength
	binary.LittleEndian.PutUint16(buf[6:8], 0)                      // Reserved

	// Symlink data
	binary.LittleEndian.PutUint16(buf[8:10], 0)                         // SubstituteNameOffset
	binary.LittleEndian.PutUint16(buf[10:12], uint16(len(targetBytes))) // SubstituteNameLength
	binary.LittleEndian.PutUint16(buf[12:14], uint16(len(targetBytes))) // PrintNameOffset
	binary.LittleEndian.PutUint16(buf[14:16], uint16(len(targetBytes))) // PrintNameLength
	binary.LittleEndian.PutUint32(buf[16:20], 1)                        // Flags (1 = relative path)

	// PathBuffer - SubstituteName followed by PrintName
	copy(buf[20:], targetBytes)
	copy(buf[20+len(targetBytes):], targetBytes)

	return buf
}

// buildIoctlResponse builds SMB2 IOCTL response [MS-SMB2] 2.2.32
func buildIoctlResponse(ctlCode uint32, fileID [16]byte, output []byte) []byte {
	// IOCTL response structure (48 bytes fixed + output):
	// - StructureSize (2 bytes) - always 49
	// - Reserved (2 bytes)
	// - CtlCode (4 bytes)
	// - FileId (16 bytes)
	// - InputOffset (4 bytes)
	// - InputCount (4 bytes)
	// - OutputOffset (4 bytes)
	// - OutputCount (4 bytes)
	// - Flags (4 bytes)
	// - Reserved2 (4 bytes)
	// - Buffer (variable)

	buf := make([]byte, 48+len(output))

	binary.LittleEndian.PutUint16(buf[0:2], 49)                    // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], 0)                     // Reserved
	binary.LittleEndian.PutUint32(buf[4:8], ctlCode)               // CtlCode
	copy(buf[8:24], fileID[:])                                     // FileId
	binary.LittleEndian.PutUint32(buf[24:28], 0)                   // InputOffset
	binary.LittleEndian.PutUint32(buf[28:32], 0)                   // InputCount
	binary.LittleEndian.PutUint32(buf[32:36], uint32(64+48))       // OutputOffset (header + response header)
	binary.LittleEndian.PutUint32(buf[36:40], uint32(len(output))) // OutputCount
	binary.LittleEndian.PutUint32(buf[40:44], 0)                   // Flags
	binary.LittleEndian.PutUint32(buf[44:48], 0)                   // Reserved2
	copy(buf[48:], output)                                         // Buffer

	return buf
}

// Cancel handles SMB2 CANCEL command [MS-SMB2] 2.2.30.
//
// Used to cancel pending operations, particularly CHANGE_NOTIFY requests.
// Per the spec, CANCEL does not send a response - the cancelled request
// is completed with STATUS_CANCELLED.
func (h *Handler) Cancel(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// CANCEL request body is just 4 bytes:
	// - StructureSize (2 bytes) = 4
	// - Reserved (2 bytes)
	if len(body) < 4 {
		logger.Debug("CANCEL: request too short", "len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("CANCEL request received",
		"sessionID", ctx.SessionID,
		"messageID", ctx.MessageID)

	// Note: CANCEL is typically used to cancel a pending CHANGE_NOTIFY
	// The MessageID in the CANCEL header identifies which request to cancel.
	// For MVP, we don't track message IDs for pending requests,
	// so we can't cancel specific requests. The watches will be cleaned up
	// when the directory handle is closed.

	// Per [MS-SMB2] 3.3.5.16: The server MUST NOT send a response to the CANCEL request.
	// The cancelled request itself should be completed with STATUS_CANCELLED by the server.
	// Returning nil ensures no SMB2 response is sent for the CANCEL command itself.
	return nil, nil
}

// ChangeNotify handles SMB2 CHANGE_NOTIFY command [MS-SMB2] 2.2.35.
//
// This command allows clients to watch directories for changes.
// For MVP, we register the watch and immediately return STATUS_PENDING.
// When changes occur (via CREATE/CLOSE/SET_INFO), we can notify watchers.
func (h *Handler) ChangeNotify(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// Parse the request
	req, err := DecodeChangeNotifyRequest(body)
	if err != nil {
		logger.Debug("CHANGE_NOTIFY: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Get the open file (must be a directory)
	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("CHANGE_NOTIFY: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Verify it's a directory
	if !openFile.IsDirectory {
		logger.Debug("CHANGE_NOTIFY: not a directory", "path", openFile.Path)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Verify session and tree match
	if openFile.SessionID != ctx.SessionID || openFile.TreeID != ctx.TreeID {
		logger.Debug("CHANGE_NOTIFY: session/tree mismatch")
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Build the watch path (share-relative)
	watchPath := openFile.Path
	if watchPath == "" {
		watchPath = "/"
	}

	// Register the pending notification if registry is available
	if h.NotifyRegistry == nil {
		logger.Debug("CHANGE_NOTIFY: NotifyRegistry not initialized")
		return NewErrorResult(types.StatusNotSupported), nil
	}

	notify := &PendingNotify{
		FileID:           req.FileID,
		SessionID:        ctx.SessionID,
		MessageID:        ctx.MessageID,
		WatchPath:        watchPath,
		ShareName:        openFile.ShareName,
		CompletionFilter: req.CompletionFilter,
		WatchTree:        req.Flags&SMB2WatchTree != 0,
		MaxOutputLength:  req.OutputBufferLength,
		AsyncCallback:    ctx.AsyncNotifyCallback,
	}

	h.NotifyRegistry.Register(notify)

	hasAsyncCallback := ctx.AsyncNotifyCallback != nil
	logger.Debug("CHANGE_NOTIFY: registered watch",
		"path", watchPath,
		"share", openFile.ShareName,
		"filter", fmt.Sprintf("0x%08X", req.CompletionFilter),
		"recursive", notify.WatchTree,
		"messageID", ctx.MessageID,
		"asyncEnabled", hasAsyncCallback)

	// Return STATUS_PENDING - the client will receive an async response when
	// a matching change occurs (if AsyncNotifyCallback is set).
	return NewErrorResult(types.StatusPending), nil
}

// OplockBreak handles SMB2 OPLOCK_BREAK acknowledgment [MS-SMB2] 2.2.24.
//
// This is called when a client acknowledges an oplock break that was initiated
// by the server due to a conflicting open by another client.
//
// **Process:**
//
//  1. Decode the break acknowledgment request
//  2. Look up the open file by FileID
//  3. Build the oplock path and acknowledge the break
//  4. Return success response
func (h *Handler) OplockBreak(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// Decode the request
	req, err := DecodeOplockBreakRequest(body)
	if err != nil {
		logger.Debug("OPLOCK_BREAK: decode error", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("OPLOCK_BREAK acknowledgment",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"newLevel", req.OplockLevel)

	// Look up the open file
	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("OPLOCK_BREAK: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Build oplock path and acknowledge the break
	oplockPath := BuildOplockPath(openFile.ShareName, openFile.Path)
	if err := h.OplockManager.AcknowledgeBreak(oplockPath, req.FileID, req.OplockLevel); err != nil {
		logger.Warn("OPLOCK_BREAK: acknowledgment failed",
			"path", openFile.Path,
			"error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Update the open file's oplock level
	openFile.OplockLevel = req.OplockLevel

	// Build success response
	resp := &OplockBreakResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     req.OplockLevel,
		FileID:          req.FileID,
	}

	respBytes, err := resp.Encode()
	if err != nil {
		logger.Error("OPLOCK_BREAK: encode error", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	logger.Debug("OPLOCK_BREAK: acknowledged",
		"path", openFile.Path,
		"newLevel", req.OplockLevel)

	return NewResult(types.StatusSuccess, respBytes), nil
}

// handleGetNtfsVolumeData handles FSCTL_GET_NTFS_VOLUME_DATA [MS-FSCC] 2.3.29.
// Returns an NTFS_VOLUME_DATA_BUFFER with VolumeSerialNumber matching the value
// used in FILE_ID_INFORMATION (0x12345678). TotalClusters and BytesPerSector
// must match FileFsFullSizeInformation values because WPTS tests verify
// consistency across all three queries.
func (h *Handler) handleGetNtfsVolumeData(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 24 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	var fileID [16]byte
	copy(fileID[:], body[8:24])

	// Get open file to access metadata handle for filesystem stats
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL FSCTL_GET_NTFS_VOLUME_DATA: invalid file ID", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Query filesystem stats so TotalClusters and BytesPerSector match
	// FileFsFullSizeInformation (WPTS checks consistency between them).
	metaSvc := h.Registry.GetMetadataService()
	totalClusters := uint64(1000000) // fallback matches FileFsFullSizeInformation fallback
	freeClusters := uint64(500000)   // fallback
	bps := uint32(bytesPerSector)    // 512 - from converters.go
	bpc := uint32(clusterSize)       // 4096 - from converters.go

	stats, err := metaSvc.GetFilesystemStatistics(ctx.Context, openFile.MetadataHandle)
	if err == nil {
		totalClusters = stats.TotalBytes / clusterSize
		freeClusters = stats.AvailableBytes / clusterSize
	}

	// Build NTFS_VOLUME_DATA_BUFFER [MS-FSCC] 2.5.1 (96 bytes)
	// Layout:
	//   VolumeSerialNumber          (8 bytes)  offset 0
	//   NumberSectors               (8 bytes)  offset 8
	//   TotalClusters               (8 bytes)  offset 16
	//   FreeClusters                (8 bytes)  offset 24
	//   TotalReserved               (8 bytes)  offset 32
	//   BytesPerSector              (4 bytes)  offset 40
	//   BytesPerCluster             (4 bytes)  offset 44
	//   BytesPerFileRecordSegment   (4 bytes)  offset 48
	//   ClustersPerFileRecordSegment(4 bytes)  offset 52
	//   MftValidDataLength          (8 bytes)  offset 56
	//   MftStartLcn                 (8 bytes)  offset 64
	//   Mft2StartLcn                (8 bytes)  offset 72
	//   MftZoneStart                (8 bytes)  offset 80
	//   MftZoneEnd                  (8 bytes)  offset 88
	const ntfsVolumeDataSize = 96
	output := make([]byte, ntfsVolumeDataSize)

	// VolumeSerialNumber must match FILE_ID_INFORMATION.VolumeSerialNumber
	binary.LittleEndian.PutUint64(output[0:8], 0x12345678)
	// NumberSectors = TotalClusters * sectorsPerUnit
	binary.LittleEndian.PutUint64(output[8:16], totalClusters*uint64(sectorsPerUnit))
	// TotalClusters - MUST match FileFsFullSizeInformation.TotalAllocationUnits
	binary.LittleEndian.PutUint64(output[16:24], totalClusters)
	// FreeClusters
	binary.LittleEndian.PutUint64(output[24:32], freeClusters)
	// TotalReserved
	binary.LittleEndian.PutUint64(output[32:40], 0)
	// BytesPerSector - MUST match FileFsFullSizeInformation.BytesPerSector
	binary.LittleEndian.PutUint32(output[40:44], bps)
	// BytesPerCluster
	binary.LittleEndian.PutUint32(output[44:48], bpc)
	// BytesPerFileRecordSegment
	binary.LittleEndian.PutUint32(output[48:52], 1024)
	// ClustersPerFileRecordSegment
	binary.LittleEndian.PutUint32(output[52:56], 0)
	// MftValidDataLength
	binary.LittleEndian.PutUint64(output[56:64], 64*1024*1024)
	// MftStartLcn
	binary.LittleEndian.PutUint64(output[64:72], 786432)
	// Mft2StartLcn
	binary.LittleEndian.PutUint64(output[72:80], 2)
	// MftZoneStart
	binary.LittleEndian.PutUint64(output[80:88], 786432)
	// MftZoneEnd
	binary.LittleEndian.PutUint64(output[88:96], 819200)

	resp := buildIoctlResponse(FsctlGetNtfsVolumeData, fileID, output)

	logger.Debug("IOCTL FSCTL_GET_NTFS_VOLUME_DATA: success",
		"volumeSerialNumber", fmt.Sprintf("0x%x", 0x12345678),
		"totalClusters", totalClusters,
		"bytesPerSector", bps)
	return NewResult(types.StatusSuccess, resp), nil
}

// handleReadFileUsnData handles FSCTL_READ_FILE_USN_DATA [MS-FSCC] 2.3.56.
// Returns a USN_RECORD for the file. Supports both V2 and V3 formats based on
// the MaxMajorVersion in the READ_FILE_USN_DATA input buffer.
// V3 is required by WPTS FSA tests for FileIdInformation validation because
// only USN_RECORD_V3 contains the 128-bit FILE_ID_128 FileReferenceNumber
// that matches FILE_ID_INFORMATION's 128-bit FileId.
func (h *Handler) handleReadFileUsnData(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 24 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	var fileID [16]byte
	copy(fileID[:], body[8:24])

	// Get open file
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL READ_FILE_USN_DATA: invalid file ID", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Get file info for attributes
	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	// Parse READ_FILE_USN_DATA input to determine requested version.
	// Input structure [MS-FSCC] 2.3.56:
	//   MinMajorVersion: WORD (2 bytes)
	//   MaxMajorVersion: WORD (2 bytes)
	// The input is in the IOCTL buffer portion (offset 56 from body start).
	inputCount := binary.LittleEndian.Uint32(body[28:32])
	maxMajorVersion := uint16(2) // Default to V2
	if inputCount >= 4 && len(body) >= 60 {
		// MinMajorVersion at buffer offset 56, MaxMajorVersion at offset 58
		maxMajorVersion = binary.LittleEndian.Uint16(body[58:60])
	}

	useV3 := maxMajorVersion >= 3

	fileNameBytes := encodeUTF16LE(openFile.FileName)
	fileAttrs := uint32(FileAttrToSMBAttributes(&file.FileAttr))

	var output []byte
	if useV3 {
		// Build USN_RECORD_V3 [MS-FSCC] 2.4.51.1
		// V3 uses FILE_ID_128 (16 bytes) for FileReferenceNumber and ParentFileReferenceNumber.
		// Layout:
		//   RecordLength (4) + MajorVersion (2) + MinorVersion (2) +
		//   FileReferenceNumber (16) + ParentFileReferenceNumber (16) +
		//   Usn (8) + TimeStamp (8) + Reason (4) + SourceInfo (4) +
		//   SecurityId (4) + FileAttributes (4) + FileNameLength (2) +
		//   FileNameOffset (2) + FileName (variable) = 76 + FileName
		const v3FixedSize = 76
		recordLen := v3FixedSize + len(fileNameBytes)
		// Pad to 8-byte boundary per MS-FSCC
		recordLen = (recordLen + 7) &^ 7

		output = make([]byte, recordLen)
		binary.LittleEndian.PutUint32(output[0:4], uint32(recordLen)) // RecordLength
		binary.LittleEndian.PutUint16(output[4:6], 3)                 // MajorVersion = 3
		binary.LittleEndian.PutUint16(output[6:8], 0)                 // MinorVersion = 0
		copy(output[8:24], file.ID[:16])                              // FileReferenceNumber (FILE_ID_128)
		// ParentFileReferenceNumber (FILE_ID_128) at offset 24-39 = zeros
		// Usn (8 bytes) at offset 40-47 = 0
		// TimeStamp (8 bytes) at offset 48-55 = 0
		// Reason (4 bytes) at offset 56-59 = 0
		// SourceInfo (4 bytes) at offset 60-63 = 0
		// SecurityId (4 bytes) at offset 64-67 = 0
		binary.LittleEndian.PutUint32(output[68:72], fileAttrs)                  // FileAttributes
		binary.LittleEndian.PutUint16(output[72:74], uint16(len(fileNameBytes))) // FileNameLength
		binary.LittleEndian.PutUint16(output[74:76], v3FixedSize)                // FileNameOffset
		copy(output[v3FixedSize:], fileNameBytes)
	} else {
		// Build USN_RECORD_V2 [MS-FSCC] 2.4.51
		const v2FixedSize = 60
		recordLen := v2FixedSize + len(fileNameBytes)
		// Pad to 8-byte boundary per MS-FSCC
		recordLen = (recordLen + 7) &^ 7

		output = make([]byte, recordLen)
		binary.LittleEndian.PutUint32(output[0:4], uint32(recordLen))                        // RecordLength
		binary.LittleEndian.PutUint16(output[4:6], 2)                                        // MajorVersion = 2
		binary.LittleEndian.PutUint16(output[6:8], 0)                                        // MinorVersion = 0
		binary.LittleEndian.PutUint64(output[8:16], binary.LittleEndian.Uint64(file.ID[:8])) // FileReferenceNumber
		binary.LittleEndian.PutUint64(output[16:24], 0)                                      // ParentFileReferenceNumber
		binary.LittleEndian.PutUint64(output[24:32], 0)                                      // Usn
		binary.LittleEndian.PutUint64(output[32:40], 0)                                      // TimeStamp
		binary.LittleEndian.PutUint32(output[40:44], 0)                                      // Reason
		binary.LittleEndian.PutUint32(output[44:48], 0)                                      // SourceInfo
		binary.LittleEndian.PutUint32(output[48:52], 0)                                      // SecurityId
		binary.LittleEndian.PutUint32(output[52:56], fileAttrs)                              // FileAttributes
		binary.LittleEndian.PutUint16(output[56:58], uint16(len(fileNameBytes)))             // FileNameLength
		binary.LittleEndian.PutUint16(output[58:60], v2FixedSize)                            // FileNameOffset
		copy(output[v2FixedSize:], fileNameBytes)
	}

	resp := buildIoctlResponse(FsctlReadFileUsnData, fileID, output)

	logger.Debug("IOCTL READ_FILE_USN_DATA: success",
		"path", openFile.Path,
		"version", map[bool]int{true: 3, false: 2}[useV3])
	return NewResult(types.StatusSuccess, resp), nil
}

// handlePipeTransceive handles FSCTL_PIPE_TRANSCEIVE for RPC over named pipes
// This is a combined write+read operation used by Windows/Linux clients for RPC [MS-FSCC] 2.3.50
func (h *Handler) handlePipeTransceive(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// IOCTL request structure [MS-SMB2] 2.2.31:
	// - StructureSize (2 bytes) - offset 0
	// - Reserved (2 bytes) - offset 2
	// - CtlCode (4 bytes) - offset 4
	// - FileId (16 bytes) - offset 8
	// - InputOffset (4 bytes) - offset 24
	// - InputCount (4 bytes) - offset 28
	// - MaxInputResponse (4 bytes) - offset 32
	// - OutputOffset (4 bytes) - offset 36
	// - OutputCount (4 bytes) - offset 40
	// - MaxOutputResponse (4 bytes) - offset 44
	// - Flags (4 bytes) - offset 48
	// - Reserved2 (4 bytes) - offset 52
	// - Buffer (variable) - offset 56
	if len(body) < 56 {
		logger.Debug("IOCTL PIPE_TRANSCEIVE: request too small", "len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	var fileID [16]byte
	copy(fileID[:], body[8:24])

	inputOffset := binary.LittleEndian.Uint32(body[24:28])
	inputCount := binary.LittleEndian.Uint32(body[28:32])
	maxOutputResponse := binary.LittleEndian.Uint32(body[44:48])

	logger.Debug("IOCTL PIPE_TRANSCEIVE",
		"fileID", fmt.Sprintf("%x", fileID),
		"inputOffset", inputOffset,
		"inputCount", inputCount,
		"maxOutputResponse", maxOutputResponse)

	// Get open file to verify it's a pipe
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL PIPE_TRANSCEIVE: invalid file ID")
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	if !openFile.IsPipe {
		logger.Debug("IOCTL PIPE_TRANSCEIVE: not a pipe",
			"path", openFile.Path)
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// Get pipe state
	pipe := h.PipeManager.GetPipe(fileID)
	if pipe == nil {
		logger.Debug("IOCTL PIPE_TRANSCEIVE: pipe not found")
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Extract input data from buffer
	// InputOffset is relative to the start of the SMB2 header (64 bytes)
	// We need to adjust for the body offset (body starts after header)
	var inputData []byte
	if inputCount > 0 {
		// The input data is in the buffer portion of the request
		// InputOffset includes SMB2 header (64 bytes), so buffer data starts at offset 56 in body
		bufferStart := uint32(56)
		if uint32(len(body)) >= bufferStart+inputCount {
			inputData = body[bufferStart : bufferStart+inputCount]
		} else {
			logger.Debug("IOCTL PIPE_TRANSCEIVE: input data out of bounds",
				"bodyLen", len(body), "bufferStart", bufferStart, "inputCount", inputCount)
			return NewErrorResult(types.StatusInvalidParameter), nil
		}
	}

	// Process the RPC transaction
	outputData, err := pipe.Transact(inputData, int(maxOutputResponse))
	if err != nil {
		logger.Debug("IOCTL PIPE_TRANSCEIVE: transact failed", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	logger.Debug("IOCTL PIPE_TRANSCEIVE: response",
		"inputLen", len(inputData), "outputLen", len(outputData))

	// Build IOCTL response
	resp := buildIoctlResponse(FsctlPipeTransceive, fileID, outputData)

	return NewResult(types.StatusSuccess, resp), nil
}

// handleValidateNegotiateInfo validates the negotiation parameters [MS-SMB2] 2.2.31.4.
//
// This FSCTL is used by SMB 3.x clients to verify that the negotiation wasn't
// tampered with by a man-in-the-middle attack. The client sends its view of
// the negotiation parameters, and the server responds with its values.
// If they don't match, the client will terminate the connection.
//
// **Request format (VALIDATE_NEGOTIATE_INFO Request):**
//
//	Offset  Size  Field
//	------  ----  ------------------
//	0       4     Capabilities
//	4       16    Guid (ClientGuid)
//	20      2     SecurityMode
//	22      2     DialectCount
//	24      2*N   Dialects
//
// **Response format (VALIDATE_NEGOTIATE_INFO Response):**
//
//	Offset  Size  Field
//	------  ----  ------------------
//	0       4     Capabilities
//	4       16    Guid (ServerGuid)
//	20      2     SecurityMode
//	22      2     Dialect (selected)
func (h *Handler) handleValidateNegotiateInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// IOCTL request structure [MS-SMB2] 2.2.31:
	// - StructureSize (2 bytes) - offset 0
	// - Reserved (2 bytes) - offset 2
	// - CtlCode (4 bytes) - offset 4
	// - FileId (16 bytes) - offset 8
	// - InputOffset (4 bytes) - offset 24
	// - InputCount (4 bytes) - offset 28
	// - MaxInputResponse (4 bytes) - offset 32
	// - OutputOffset (4 bytes) - offset 36
	// - OutputCount (4 bytes) - offset 40
	// - MaxOutputResponse (4 bytes) - offset 44
	// - Flags (4 bytes) - offset 48
	// - Reserved2 (4 bytes) - offset 52
	// - Buffer (variable) - offset 56
	if len(body) < 56 {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: request too small", "len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	var fileID [16]byte
	copy(fileID[:], body[8:24])

	// Per [MS-SMB2] 2.2.31.4, FSCTL_VALIDATE_NEGOTIATE_INFO MUST use FileId
	// {0xFFFFFFFFFFFFFFFF, 0xFFFFFFFFFFFFFFFF} (all 0xFF bytes).
	if !bytes.Equal(fileID[:], allFFFileID) {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: unexpected FileId (expected all 0xFF)", "fileID", fileID)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	inputCount := binary.LittleEndian.Uint32(body[28:32])

	// Minimum input size: 24 bytes (Capabilities + Guid + SecurityMode + DialectCount)
	if inputCount < 24 {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: input too small", "inputCount", inputCount)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Extract input data from buffer portion
	bufferStart := uint32(56)
	if uint32(len(body)) < bufferStart+inputCount {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: input data out of bounds",
			"bodyLen", len(body), "bufferStart", bufferStart, "inputCount", inputCount)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	inputData := body[bufferStart : bufferStart+inputCount]

	// Parse VALIDATE_NEGOTIATE_INFO request
	// clientCapabilities := binary.LittleEndian.Uint32(inputData[0:4])
	// clientGuid := inputData[4:20]
	// clientSecurityMode := binary.LittleEndian.Uint16(inputData[20:22])
	dialectCount := binary.LittleEndian.Uint16(inputData[22:24])

	// Validate dialect count
	expectedSize := 24 + (int(dialectCount) * 2)
	if int(inputCount) < expectedSize {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: not enough dialects",
			"dialectCount", dialectCount, "inputCount", inputCount, "expectedSize", expectedSize)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse dialects and re-determine what we would negotiate
	// This uses the same logic as Negotiate handler
	var selectedDialect types.Dialect
	for i := uint16(0); i < dialectCount; i++ {
		offset := 24 + (int(i) * 2)
		dialect := types.Dialect(binary.LittleEndian.Uint16(inputData[offset : offset+2]))

		switch dialect {
		case types.SMB2Dialect0210:
			// SMB 2.1 is our highest supported dialect
			if selectedDialect < types.SMB2Dialect0210 {
				selectedDialect = types.SMB2Dialect0210
			}
		case types.SMB2Dialect0202, types.SMB2DialectWild:
			// SMB 2.0.2 is our baseline
			if selectedDialect < types.SMB2Dialect0202 {
				selectedDialect = types.SMB2Dialect0202
			}
		}
	}

	if selectedDialect == 0 {
		// No common dialect found - this shouldn't happen if negotiation succeeded
		logger.Warn("IOCTL VALIDATE_NEGOTIATE_INFO: no common dialect")
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Build SecurityMode based on signing configuration
	// Bit 0 (0x01): SMB2_NEGOTIATE_SIGNING_ENABLED
	// Bit 1 (0x02): SMB2_NEGOTIATE_SIGNING_REQUIRED
	var securityMode uint16
	if h.SigningConfig.Enabled {
		securityMode |= 0x01
	}
	if h.SigningConfig.Required {
		securityMode |= 0x02
	}

	// Build VALIDATE_NEGOTIATE_INFO response (24 bytes)
	output := make([]byte, 24)
	binary.LittleEndian.PutUint32(output[0:4], 0) // Capabilities (none)
	copy(output[4:20], h.ServerGUID[:])           // ServerGuid
	binary.LittleEndian.PutUint16(output[20:22], securityMode)
	binary.LittleEndian.PutUint16(output[22:24], uint16(selectedDialect))

	logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: success",
		"dialect", selectedDialect.String(),
		"securityMode", fmt.Sprintf("0x%02X", securityMode))

	// Build IOCTL response
	resp := buildIoctlResponse(FsctlValidateNegotiateInfo, fileID, output)

	return NewResult(types.StatusSuccess, resp), nil
}
