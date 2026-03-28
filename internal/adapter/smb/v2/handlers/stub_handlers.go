package handlers

import (
	"bytes"
	"fmt"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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
	FsctlIsPathnameValid        uint32 = 0x0009002C // [MS-FSCC] 2.3.33 - Pathname validation
	FsctlGetNtfsVolumeData      uint32 = 0x00090064 // [MS-FSCC] 2.3.29 - NTFS volume data
	FsctlReadFileUsnData        uint32 = 0x000900EB // [MS-FSCC] 2.3.56 - Read file USN data
	FsctlGetCompression         uint32 = 0x0009003C // [MS-FSCC] 2.3.9 - Get compression state
	FsctlSetCompression         uint32 = 0x0009C040 // [MS-FSCC] 2.3.53 - Set compression state
	FsctlGetIntegrityInfo       uint32 = 0x0009027C // [MS-FSCC] 2.3.25 - Get integrity information
	FsctlSetIntegrityInfo       uint32 = 0x0009C280 // [MS-FSCC] 2.3.55 - Set integrity information (WPTS uses READ|WRITE access)
	FsctlCreateOrGetObjectID    uint32 = 0x000900C0 // [MS-FSCC] 2.3.7 - Create or get object ID
	FsctlGetObjectID            uint32 = 0x0009009C // [MS-FSCC] 2.3.28 - Get object ID
	FsctlMarkHandle             uint32 = 0x000900FC // [MS-FSCC] 2.3.36 - Mark handle
	FsctlQueryFileRegions       uint32 = 0x00090284 // [MS-FSCC] 2.3.51 - Query file regions
)

// Reparse point constants [MS-FSCC] 2.1.2.1
const (
	IoReparseTagSymlink uint32 = 0xA000000C
)

// handleGetReparsePoint handles FSCTL_GET_REPARSE_POINT for readlink
func (h *Handler) handleGetReparsePoint(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Get open file
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL GET_REPARSE_POINT: file handle not found (closed)", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusFileClosed), nil
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
	tw := smbenc.NewWriter(len(targetUTF16) * 2)
	for _, r := range targetUTF16 {
		tw.WriteUint16(r)
	}
	targetBytes := tw.Bytes()

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

	w := smbenc.NewWriter(8 + reparseDataLen)
	// Header
	w.WriteUint32(IoReparseTagSymlink)    // ReparseTag
	w.WriteUint16(uint16(reparseDataLen)) // ReparseDataLength
	w.WriteUint16(0)                      // Reserved

	// Symlink data
	w.WriteUint16(0)                        // SubstituteNameOffset
	w.WriteUint16(uint16(len(targetBytes))) // SubstituteNameLength
	w.WriteUint16(uint16(len(targetBytes))) // PrintNameOffset
	w.WriteUint16(uint16(len(targetBytes))) // PrintNameLength
	w.WriteUint32(1)                        // Flags (1 = relative path)

	// PathBuffer - SubstituteName followed by PrintName
	w.WriteBytes(targetBytes)
	w.WriteBytes(targetBytes)

	return w.Bytes()
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

	w := smbenc.NewWriter(48 + len(output))
	w.WriteUint16(49)                  // StructureSize
	w.WriteUint16(0)                   // Reserved
	w.WriteUint32(ctlCode)             // CtlCode
	w.WriteBytes(fileID[:])            // FileId
	w.WriteUint32(0)                   // InputOffset
	w.WriteUint32(0)                   // InputCount
	w.WriteUint32(uint32(64 + 48))     // OutputOffset (header + response header)
	w.WriteUint32(uint32(len(output))) // OutputCount
	w.WriteUint32(0)                   // Flags
	w.WriteUint32(0)                   // Reserved2
	w.WriteBytes(output)               // Buffer

	return w.Bytes()
}

// Cancel handles SMB2 CANCEL command [MS-SMB2] 2.2.30.
//
// Used to cancel pending operations, particularly CHANGE_NOTIFY requests.
// Per the spec, CANCEL does not send a response - the cancelled request
// is completed with STATUS_CANCELLED.
//
// Per [MS-SMB2] 3.3.5.16:
//   - If the request has SMB2_FLAGS_ASYNC_COMMAND set, use AsyncId to find the request
//   - Otherwise, use MessageID to find the request
//   - The cancelled request is completed with STATUS_CANCELLED
//   - The CANCEL command itself gets no response
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
		"messageID", ctx.MessageID,
		"requestAsyncId", ctx.RequestAsyncId)

	// Try to cancel a pending CHANGE_NOTIFY request
	if h.NotifyRegistry != nil {
		var cancelled *PendingNotify

		// Per [MS-SMB2] 3.3.5.16: If SMB2_FLAGS_ASYNC_COMMAND is set,
		// look up by AsyncId; otherwise by MessageID.
		if ctx.RequestAsyncId != 0 {
			cancelled = h.NotifyRegistry.UnregisterByAsyncId(ctx.RequestAsyncId)
		} else {
			cancelled = h.NotifyRegistry.UnregisterByMessageID(ctx.MessageID)
		}

		if cancelled != nil {
			logger.Debug("CANCEL: cancelled pending CHANGE_NOTIFY",
				"watchPath", cancelled.WatchPath,
				"asyncId", cancelled.AsyncId,
				"messageID", cancelled.MessageID)

			// Send STATUS_CANCELLED for the original CHANGE_NOTIFY request
			// via the async callback. This completes the pending request.
			if cancelled.AsyncCallback != nil {
				cancelResp := &ChangeNotifyResponse{
					SMBResponseBase: SMBResponseBase{Status: types.StatusCancelled},
				}
				if err := cancelled.AsyncCallback(cancelled.SessionID, cancelled.MessageID, cancelled.AsyncId, cancelResp); err != nil {
					logger.Warn("CANCEL: failed to send STATUS_CANCELLED",
						"messageID", cancelled.MessageID,
						"error", err)
				}
			}
		} else {
			logger.Debug("CANCEL: no pending request found to cancel",
				"asyncId", ctx.RequestAsyncId,
				"messageID", ctx.MessageID)
		}
	}

	// Per [MS-SMB2] 3.3.5.16: The server MUST NOT send a response to the CANCEL request.
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
		logger.Debug("CHANGE_NOTIFY: file handle not found (closed)", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// Verify it's a directory
	if !openFile.IsDirectory {
		logger.Debug("CHANGE_NOTIFY: not a directory", "path", openFile.Path)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Per MS-SMB2 3.3.5.15: CompletionFilter must contain valid flags.
	// Reject requests with no flags or invalid flags.
	if !IsValidCompletionFilter(req.CompletionFilter) {
		logger.Debug("CHANGE_NOTIFY: invalid CompletionFilter",
			"filter", fmt.Sprintf("0x%08X", req.CompletionFilter))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Per MS-SMB2 3.3.5.15: If OutputBufferLength exceeds MaxTransactSize,
	// the server MUST fail the request with STATUS_INVALID_PARAMETER.
	if req.OutputBufferLength > h.MaxTransactSize {
		logger.Debug("CHANGE_NOTIFY: OutputBufferLength exceeds MaxTransactSize",
			"outputBufferLength", req.OutputBufferLength,
			"maxTransactSize", h.MaxTransactSize)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Per MS-SMB2 3.3.5.15: The directory handle must have been opened
	// with FILE_LIST_DIRECTORY (0x0001) access. Generic rights (GENERIC_READ,
	// GENERIC_ALL, MAXIMUM_ALLOWED) implicitly include FILE_LIST_DIRECTORY
	// for directories, so we check both specific and generic forms.
	const listDirMask = 0x00000001 | 0x80000000 | 0x10000000 | 0x02000000 // FILE_LIST_DIRECTORY | GENERIC_READ | GENERIC_ALL | MAXIMUM_ALLOWED
	hasListDir := openFile.DesiredAccess&listDirMask != 0
	if !hasListDir {
		logger.Debug("CHANGE_NOTIFY: missing FILE_LIST_DIRECTORY access",
			"path", openFile.Path,
			"desiredAccess", fmt.Sprintf("0x%x", openFile.DesiredAccess))
		return NewErrorResult(types.StatusAccessDenied), nil
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

	// Generate a unique AsyncId for this pending request.
	// Per MS-SMB2 3.3.5.15, the server assigns an AsyncId and sends an
	// interim response with STATUS_PENDING. The same AsyncId is used in
	// the final async response when the notification is delivered.
	asyncId := h.generateAsyncId()

	notify := &PendingNotify{
		FileID:           req.FileID,
		SessionID:        ctx.SessionID,
		MessageID:        ctx.MessageID,
		AsyncId:          asyncId,
		WatchPath:        watchPath,
		ShareName:        openFile.ShareName,
		CompletionFilter: req.CompletionFilter,
		WatchTree:        req.Flags&SMB2WatchTree != 0,
		MaxOutputLength:  req.OutputBufferLength,
		AsyncCallback:    ctx.AsyncNotifyCallback,
	}

	if err := h.NotifyRegistry.Register(notify); err != nil {
		logger.Warn("CHANGE_NOTIFY: rejected — too many pending watches",
			"path", watchPath,
			"sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusInsufficientResources), nil
	}

	hasAsyncCallback := ctx.AsyncNotifyCallback != nil
	logger.Debug("CHANGE_NOTIFY: registered watch",
		"path", watchPath,
		"share", openFile.ShareName,
		"filter", fmt.Sprintf("0x%08X", req.CompletionFilter),
		"recursive", notify.WatchTree,
		"messageID", ctx.MessageID,
		"asyncId", asyncId,
		"asyncEnabled", hasAsyncCallback)

	// Return STATUS_PENDING with AsyncId - the client will receive an
	// interim response with SMB2_FLAGS_ASYNC_COMMAND set and this AsyncId.
	// When a matching change occurs, the final async response uses the same AsyncId.
	return &HandlerResult{
		Status:  types.StatusPending,
		Data:    nil,
		AsyncId: asyncId,
	}, nil
}

// OplockBreak handles SMB2 OPLOCK_BREAK acknowledgment [MS-SMB2] 2.2.24.
//
// Supports both:
//   - Lease break acks (StructureSize=36): decoded and delegated to LeaseManager
//   - Traditional oplock break acks (StructureSize=24): the FileID is used to
//     reconstruct the synthetic lease key, then delegated to LeaseManager
//
// **Process:**
//
//  1. Read StructureSize to determine oplock vs lease break
//  2. For lease (36 bytes): decode lease key + state, delegate to LeaseManager
//  3. For traditional (24 bytes): look up open file, derive synthetic lease key, delegate
func (h *Handler) OplockBreak(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 2 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Read StructureSize to determine oplock vs lease break ack
	structSize := uint16(body[0]) | uint16(body[1])<<8

	if structSize == LeaseBreakAckSize {
		return h.handleLeaseBreakAck(ctx, body)
	}

	// Traditional oplock break ack (StructureSize=24) [MS-SMB2] 2.2.24.1
	// Traditional oplocks are internally mapped to leases. Reconstruct the
	// synthetic lease key from the FileID and delegate to LeaseManager.
	return h.handleOplockBreakAck(ctx, body)
}

// handleLeaseBreakAck handles an SMB2 Lease Break Acknowledgment [MS-SMB2] 2.2.24.2.
func (h *Handler) handleLeaseBreakAck(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	ack, err := DecodeLeaseBreakAcknowledgment(body)
	if err != nil {
		logger.Debug("LEASE_BREAK_ACK: decode error", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("LEASE_BREAK_ACK acknowledgment",
		"leaseKey", fmt.Sprintf("%x", ack.LeaseKey),
		"acknowledgedState", lock.LeaseStateToString(ack.LeaseState))

	if h.LeaseManager == nil {
		logger.Warn("LEASE_BREAK_ACK: no lease manager")
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	if err := h.LeaseManager.AcknowledgeLeaseBreak(ctx.Context, ack.LeaseKey, ack.LeaseState, 0); err != nil {
		logger.Warn("LEASE_BREAK_ACK: acknowledgment failed",
			"leaseKey", fmt.Sprintf("%x", ack.LeaseKey),
			"error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Build lease break response
	respBytes := EncodeLeaseBreakResponse(ack.LeaseKey, ack.LeaseState)

	logger.Debug("LEASE_BREAK_ACK: acknowledged",
		"leaseKey", fmt.Sprintf("%x", ack.LeaseKey),
		"newState", lock.LeaseStateToString(ack.LeaseState))

	return NewResult(types.StatusSuccess, respBytes), nil
}

// handleOplockBreakAck handles a traditional SMB2 OPLOCK_BREAK acknowledgment [MS-SMB2] 2.2.24.1.
//
// Traditional oplocks are internally mapped to leases via synthetic lease keys.
// This handler:
//  1. Decodes the 24-byte oplock break ack (extracts FileID and new oplock level)
//  2. Looks up the OpenFile to find its synthetic lease key
//  3. Maps the acknowledged oplock level to a lease state
//  4. Delegates to LeaseManager.AcknowledgeLeaseBreak
//  5. Returns a 24-byte oplock break response
func (h *Handler) handleOplockBreakAck(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	ack, err := DecodeOplockBreakRequest(body)
	if err != nil {
		logger.Debug("OPLOCK_BREAK_ACK: decode error", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("OPLOCK_BREAK_ACK: traditional oplock acknowledgment",
		"fileID", fmt.Sprintf("%x", ack.FileID),
		"newLevel", oplockLevelName(ack.OplockLevel))

	// Look up the open file to find its lease key
	openFile, ok := h.GetOpenFile(ack.FileID)
	if !ok {
		logger.Debug("OPLOCK_BREAK_ACK: file not found", "fileID", fmt.Sprintf("%x", ack.FileID))
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// The synthetic lease key was stored on the OpenFile during CREATE
	if openFile.LeaseKey == ([16]byte{}) {
		logger.Debug("OPLOCK_BREAK_ACK: no lease key on open file", "fileID", fmt.Sprintf("%x", ack.FileID))
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	if h.LeaseManager == nil {
		logger.Warn("OPLOCK_BREAK_ACK: no lease manager")
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Map acknowledged oplock level to lease state
	newState := oplockLevelToLeaseState(ack.OplockLevel)

	if err := h.LeaseManager.AcknowledgeLeaseBreak(ctx.Context, openFile.LeaseKey, newState, 0); err != nil {
		logger.Warn("OPLOCK_BREAK_ACK: acknowledgment failed",
			"fileID", fmt.Sprintf("%x", ack.FileID),
			"error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Update the oplock level on the open file
	openFile.OplockLevel = ack.OplockLevel

	// Build oplock break response (24 bytes)
	resp := &OplockBreakResponse{
		OplockLevel: ack.OplockLevel,
		FileID:      ack.FileID,
	}
	respBytes, err := resp.Encode()
	if err != nil {
		logger.Error("OPLOCK_BREAK_ACK: encode error", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	logger.Debug("OPLOCK_BREAK_ACK: acknowledged",
		"fileID", fmt.Sprintf("%x", ack.FileID),
		"newLevel", oplockLevelName(ack.OplockLevel))

	return NewResult(types.StatusSuccess, respBytes), nil
}

// handleGetNtfsVolumeData handles FSCTL_GET_NTFS_VOLUME_DATA [MS-FSCC] 2.3.29.
// Returns an NTFS_VOLUME_DATA_BUFFER with VolumeSerialNumber matching the value
// used in FILE_ID_INFORMATION (ntfsVolumeSerialNumber). TotalClusters and BytesPerSector
// must match FileFsFullSizeInformation values because WPTS tests verify
// consistency across all three queries.
func (h *Handler) handleGetNtfsVolumeData(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Get open file to access metadata handle for filesystem stats
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL FSCTL_GET_NTFS_VOLUME_DATA: file handle not found (closed)", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusFileClosed), nil
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
	const ntfsVolumeDataSize = 96
	w := smbenc.NewWriter(ntfsVolumeDataSize)
	w.WriteUint64(ntfsVolumeSerialNumber)                 // VolumeSerialNumber
	w.WriteUint64(totalClusters * uint64(sectorsPerUnit)) // NumberSectors
	w.WriteUint64(totalClusters)                          // TotalClusters
	w.WriteUint64(freeClusters)                           // FreeClusters
	w.WriteUint64(0)                                      // TotalReserved
	w.WriteUint32(bps)                                    // BytesPerSector
	w.WriteUint32(bpc)                                    // BytesPerCluster
	w.WriteUint32(1024)                                   // BytesPerFileRecordSegment
	w.WriteUint32(0)                                      // ClustersPerFileRecordSegment
	w.WriteUint64(64 * 1024 * 1024)                       // MftValidDataLength
	w.WriteUint64(786432)                                 // MftStartLcn
	w.WriteUint64(2)                                      // Mft2StartLcn
	w.WriteUint64(786432)                                 // MftZoneStart
	w.WriteUint64(819200)                                 // MftZoneEnd

	resp := buildIoctlResponse(FsctlGetNtfsVolumeData, fileID, w.Bytes())

	logger.Debug("IOCTL FSCTL_GET_NTFS_VOLUME_DATA: success",
		"volumeSerialNumber", fmt.Sprintf("0x%x", ntfsVolumeSerialNumber),
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
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Get open file
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL READ_FILE_USN_DATA: file handle not found (closed)", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusFileClosed), nil
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
	// Use a separate reader at offset 28 for inputCount
	inputR := smbenc.NewReader(body[28:32])
	inputCount := inputR.ReadUint32()
	maxMajorVersion := uint16(2) // Default to V2
	if inputCount >= 4 && len(body) >= 60 {
		// MinMajorVersion at buffer offset 56, MaxMajorVersion at offset 58
		versionR := smbenc.NewReader(body[58:60])
		maxMajorVersion = versionR.ReadUint16()
	}

	useV3 := maxMajorVersion >= 3

	fileNameBytes := encodeUTF16LE(openFile.FileName)
	fileAttrs := uint32(FileAttrToSMBAttributes(&file.FileAttr))

	// Note: Usn, TimeStamp, Reason, SourceInfo, SecurityId are stub zeros.
	// Real NTFS populates these from the USN journal. Sufficient for WPTS conformance
	// but would need real values if clients rely on USN journal functionality.
	var output []byte
	if useV3 {
		// Build USN_RECORD_V3 [MS-FSCC] 2.4.51.1
		const v3FixedSize = 76
		recordLen := v3FixedSize + len(fileNameBytes)
		// Pad to 8-byte boundary per MS-FSCC
		recordLen = (recordLen + 7) &^ 7

		w := smbenc.NewWriter(recordLen)
		w.WriteUint32(uint32(recordLen))          // RecordLength
		w.WriteUint16(3)                          // MajorVersion = 3
		w.WriteUint16(0)                          // MinorVersion = 0
		w.WriteBytes(file.ID[:16])                // FileReferenceNumber (FILE_ID_128)
		w.WriteZeros(16)                          // ParentFileReferenceNumber
		w.WriteUint64(0)                          // Usn
		w.WriteUint64(0)                          // TimeStamp
		w.WriteUint32(0)                          // Reason
		w.WriteUint32(0)                          // SourceInfo
		w.WriteUint32(0)                          // SecurityId
		w.WriteUint32(fileAttrs)                  // FileAttributes
		w.WriteUint16(uint16(len(fileNameBytes))) // FileNameLength
		w.WriteUint16(v3FixedSize)                // FileNameOffset
		w.WriteBytes(fileNameBytes)
		// Pad to 8-byte boundary
		w.Pad(8)
		output = w.Bytes()
	} else {
		// Build USN_RECORD_V2 [MS-FSCC] 2.4.51
		const v2FixedSize = 60
		recordLen := v2FixedSize + len(fileNameBytes)
		// Pad to 8-byte boundary per MS-FSCC
		recordLen = (recordLen + 7) &^ 7

		w := smbenc.NewWriter(recordLen)
		w.WriteUint32(uint32(recordLen)) // RecordLength
		w.WriteUint16(2)                 // MajorVersion = 2
		w.WriteUint16(0)                 // MinorVersion = 0
		idR := smbenc.NewReader(file.ID[:8])
		w.WriteUint64(idR.ReadUint64())           // FileReferenceNumber
		w.WriteUint64(0)                          // ParentFileReferenceNumber
		w.WriteUint64(0)                          // Usn
		w.WriteUint64(0)                          // TimeStamp
		w.WriteUint32(0)                          // Reason
		w.WriteUint32(0)                          // SourceInfo
		w.WriteUint32(0)                          // SecurityId
		w.WriteUint32(fileAttrs)                  // FileAttributes
		w.WriteUint16(uint16(len(fileNameBytes))) // FileNameLength
		w.WriteUint16(v2FixedSize)                // FileNameOffset
		w.WriteBytes(fileNameBytes)
		// Pad to 8-byte boundary
		w.Pad(8)
		output = w.Bytes()
	}

	resp := buildIoctlResponse(FsctlReadFileUsnData, fileID, output)

	usnVersion := 2
	if useV3 {
		usnVersion = 3
	}
	logger.Debug("IOCTL READ_FILE_USN_DATA: success",
		"path", openFile.Path,
		"version", usnVersion)
	return NewResult(types.StatusSuccess, resp), nil
}

// handlePipeTransceive handles FSCTL_PIPE_TRANSCEIVE for RPC over named pipes
// This is a combined write+read operation used by Windows/Linux clients for RPC [MS-FSCC] 2.3.50
func (h *Handler) handlePipeTransceive(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 56 {
		logger.Debug("IOCTL PIPE_TRANSCEIVE: request too small", "len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	r := smbenc.NewReader(body)
	r.Skip(4) // StructureSize(2) + Reserved(2)
	r.Skip(4) // CtlCode
	var fileID [16]byte
	copy(fileID[:], r.ReadBytes(16))    // FileId
	inputOffset := r.ReadUint32()       // InputOffset
	inputCount := r.ReadUint32()        // InputCount
	r.Skip(4)                           // MaxInputResponse
	r.Skip(4)                           // OutputOffset
	r.Skip(4)                           // OutputCount
	maxOutputResponse := r.ReadUint32() // MaxOutputResponse
	if r.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("IOCTL PIPE_TRANSCEIVE",
		"fileID", fmt.Sprintf("%x", fileID),
		"inputOffset", inputOffset,
		"inputCount", inputCount,
		"maxOutputResponse", maxOutputResponse)

	// Get open file to verify it's a pipe
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL PIPE_TRANSCEIVE: file handle not found (closed)")
		return NewErrorResult(types.StatusFileClosed), nil
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
