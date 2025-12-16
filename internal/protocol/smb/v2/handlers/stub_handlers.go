package handlers

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Common IOCTL/FSCTL codes [MS-FSCC] 2.3
const (
	FsctlDfsGetReferrals        uint32 = 0x00060194
	FsctlPipeWait               uint32 = 0x00110018
	FsctlValidateNegotiateInfo  uint32 = 0x00140204
	FsctlQueryNetworkInterfInfo uint32 = 0x001401FC
	FsctlPipePeek               uint32 = 0x0011400C
	FsctlSrvEnumerateSnapshots  uint32 = 0x00144064
	FsctlSrvRequestResumeKey    uint32 = 0x00140078
	FsctlSrvCopyChunk           uint32 = 0x001440F2
	FsctlSrvCopyChunkWrite      uint32 = 0x001480F2
	FsctlGetReparsePoint        uint32 = 0x000900A8 // For readlink [MS-FSCC] 2.3.30
)

// Reparse point constants [MS-FSCC] 2.1.2.1
const (
	IoReparseTagSymlink uint32 = 0xA000000C
)

// Ioctl handles SMB2 IOCTL command [MS-SMB2] 2.2.31, 2.2.32
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
		// This validates the negotiation - return NOT_SUPPORTED to skip validation
		// macOS will proceed without it
		logger.Debug("IOCTL FSCTL_VALIDATE_NEGOTIATE_INFO - not supported")
		return NewErrorResult(types.StatusNotSupported), nil

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

	// Get metadata store
	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("IOCTL GET_REPARSE_POINT: failed to get metadata store", "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// Build auth context
	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("IOCTL GET_REPARSE_POINT: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// Read symlink target
	target, _, err := metadataStore.ReadSymlink(authCtx, openFile.MetadataHandle)
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

// Cancel handles SMB2 CANCEL command [MS-SMB2] 2.2.30
// Used to cancel pending operations.
func (h *Handler) Cancel(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("CANCEL request (not implemented)")
	// Cancel doesn't send a response in most cases
	return NewErrorResult(types.StatusCancelled), nil
}

// ChangeNotify handles SMB2 CHANGE_NOTIFY command [MS-SMB2] 2.2.35
// Returns STATUS_NOT_SUPPORTED for Phase 1.
func (h *Handler) ChangeNotify(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("CHANGE_NOTIFY request (not implemented)")
	return NewErrorResult(types.StatusNotSupported), nil
}

// Lock handles SMB2 LOCK command [MS-SMB2] 2.2.26
// Returns STATUS_NOT_SUPPORTED for Phase 1.
func (h *Handler) Lock(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("LOCK request (not implemented)")
	return NewErrorResult(types.StatusNotSupported), nil
}

// OplockBreak handles SMB2 OPLOCK_BREAK command [MS-SMB2] 2.2.23, 2.2.24
// Returns STATUS_NOT_SUPPORTED for Phase 1.
func (h *Handler) OplockBreak(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("OPLOCK_BREAK request (not implemented)")
	return NewErrorResult(types.StatusNotSupported), nil
}
