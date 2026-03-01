package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// IOCTLHandler is the function signature for IOCTL sub-handlers.
// Each handler receives the Handler instance, per-request context, and the
// full IOCTL request body (starting at StructureSize, after the SMB2 header).
type IOCTLHandler func(h *Handler, ctx *SMBHandlerContext, body []byte) (*HandlerResult, error)

// ioctlDispatch maps FSCTL control codes to their handlers.
// Populated in init() so that adding new handlers only requires a new entry.
var ioctlDispatch map[uint32]IOCTLHandler

func init() {
	ioctlDispatch = map[uint32]IOCTLHandler{
		FsctlValidateNegotiateInfo: (*Handler).handleValidateNegotiateInfo,
		FsctlGetReparsePoint:       (*Handler).handleGetReparsePoint,
		FsctlPipeTransceive:        (*Handler).handlePipeTransceive,
		FsctlGetNtfsVolumeData:     (*Handler).handleGetNtfsVolumeData,
		FsctlReadFileUsnData:       (*Handler).handleReadFileUsnData,
		FsctlSrvEnumerateSnapshots: (*Handler).handleEnumerateSnapshots,
	}
}

// Ioctl handles the SMB2 IOCTL command [MS-SMB2] 2.2.31, 2.2.32.
// It dispatches filesystem control codes via a map-based dispatch table.
// Unsupported FSCTLs return StatusNotSupported gracefully.
func (h *Handler) Ioctl(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// Read CtlCode at offset 4 (past StructureSize(2) + Reserved(2))
	r := smbenc.NewReader(body)
	r.Skip(4) // StructureSize(2) + Reserved(2)
	ctlCode := r.ReadUint32()
	if r.Err() != nil {
		logger.Debug("IOCTL request too small", "len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("IOCTL request",
		"ctlCode", fmt.Sprintf("0x%08X", ctlCode),
		"bodyLen", len(body))

	handler, ok := ioctlDispatch[ctlCode]
	if !ok {
		logger.Debug("IOCTL unknown control code - not supported",
			"ctlCode", fmt.Sprintf("0x%08X", ctlCode))
		return NewErrorResult(types.StatusNotSupported), nil
	}

	return handler(h, ctx, body)
}

// parseIoctlFileID extracts the 16-byte FileID from an IOCTL request body.
// The IOCTL request layout is: StructureSize(2) + Reserved(2) + CtlCode(4) + FileId(16),
// so FileID starts at offset 8 and requires at least 24 bytes.
func parseIoctlFileID(body []byte) ([16]byte, bool) {
	var fileID [16]byte
	if len(body) < 24 {
		return fileID, false
	}
	copy(fileID[:], body[8:24])
	return fileID, true
}

// handleEnumerateSnapshots handles FSCTL_SRV_ENUMERATE_SNAPSHOTS [MS-SMB2] 2.2.32.2.
// Returns empty snapshot list so Windows "Previous Versions" tab shows
// "no previous versions" instead of an error.
func (h *Handler) handleEnumerateSnapshots(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("IOCTL FSCTL_SRV_ENUMERATE_SNAPSHOTS: returning empty snapshot list")
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	// SRV_SNAPSHOT_ARRAY: NumberOfSnapshots(4) + NumberOfSnapshotsReturned(4) + SnapshotArraySize(4)
	output := make([]byte, 12)
	resp := buildIoctlResponse(FsctlSrvEnumerateSnapshots, fileID, output)
	return NewResult(types.StatusSuccess, resp), nil
}
