package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
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

	default:
		logger.Debug("IOCTL unknown control code - not supported",
			"ctlCode", fmt.Sprintf("0x%08X", ctlCode))
		return NewErrorResult(types.StatusNotSupported), nil
	}
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
