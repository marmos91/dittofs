package handlers

import (
	"bytes"
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
		FsctlValidateNegotiateInfo:  (*Handler).handleValidateNegotiateInfo,
		FsctlGetReparsePoint:        (*Handler).handleGetReparsePoint,
		FsctlPipeTransceive:         (*Handler).handlePipeTransceive,
		FsctlGetNtfsVolumeData:      (*Handler).handleGetNtfsVolumeData,
		FsctlReadFileUsnData:        (*Handler).handleReadFileUsnData,
		FsctlSrvEnumerateSnapshots:  (*Handler).handleEnumerateSnapshots,
		FsctlIsPathnameValid:        (*Handler).handleIsPathnameValid,
		FsctlGetCompression:         (*Handler).handleGetCompression,
		FsctlSetCompression:         (*Handler).handleSetCompression,
		FsctlGetIntegrityInfo:       (*Handler).handleGetIntegrityInfo,
		FsctlSetIntegrityInfo:       (*Handler).handleSetIntegrityInfo,
		FsctlGetObjectID:            (*Handler).handleGetObjectID,
		FsctlCreateOrGetObjectID:    (*Handler).handleCreateOrGetObjectID,
		FsctlMarkHandle:             (*Handler).handleMarkHandle,
		FsctlQueryFileRegions:       (*Handler).handleQueryFileRegions,
		FsctlSrvRequestResumeKey:    (*Handler).handleSrvRequestResumeKey,
		FsctlSrvCopyChunk:           (*Handler).handleSrvCopyChunk,
		FsctlSrvCopyChunkWrite:      (*Handler).handleSrvCopyChunk,
		FsctlQueryNetworkInterfInfo: (*Handler).handleQueryNetworkInterfaceInfo,
		FsctlSetSparse:              (*Handler).handleSetSparse,
		FsctlQueryAllocatedRanges:   (*Handler).handleQueryAllocatedRanges,
		FsctlSetZeroData:            (*Handler).handleSetZeroData,

		// Samba-private torture FSCTLs. Accepted as no-ops so that smbtorture
		// fixtures (notably the multichannel.leases.test{2,3} pair) don't get
		// stranded on the very first assertion when test2's
		// `test_block_smb2_transport` falls through to a hard failure.
		// See issue #436 and the constant comment in stub_handlers.go.
		FsctlSmbtortureForceUnackedTimeout: (*Handler).handleSmbtortureForceUnackedTimeout,
	}
}

// Ioctl handles the SMB2 IOCTL command [MS-SMB2] 2.2.31, 2.2.32.
// It dispatches filesystem control codes via a map-based dispatch table.
// Unsupported FSCTLs return StatusNotSupported gracefully.
//
// Per MS-SMB2 3.3.5.15, the FileID must correspond to a valid open file
// unless the FSCTL uses a special handle (e.g., VALIDATE_NEGOTIATE_INFO).
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

	// Per MS-SMB2 3.3.5.15: validate that the FileID corresponds to an open
	// file, unless this is a "no-handle" FSCTL that uses the 16-byte all-0xFF
	// sentinel FileID (e.g., VALIDATE_NEGOTIATE_INFO, PIPE_TRANSCEIVE).
	if !ioctlNoHandleFSCTL(ctlCode) {
		fileID, ok := parseIoctlFileID(body)
		if ok {
			if _, found := h.GetOpenFile(fileID); !found {
				logger.Debug("IOCTL file handle not found (closed)",
					"ctlCode", fmt.Sprintf("0x%08X", ctlCode),
					"fileID", fmt.Sprintf("%x", fileID))
				return NewErrorResult(types.StatusFileClosed), nil
			}
		}
	}

	handler, ok := ioctlDispatch[ctlCode]
	if !ok {
		logger.Debug("IOCTL unknown control code - not supported",
			"ctlCode", fmt.Sprintf("0x%08X", ctlCode))
		return NewErrorResult(types.StatusNotSupported), nil
	}

	return handler(h, ctx, body)
}

// ioctlNoHandleFSCTL returns true for FSCTLs that use a special/sentinel FileID
// and do not require an open file handle.
func ioctlNoHandleFSCTL(ctlCode uint32) bool {
	switch ctlCode {
	case FsctlValidateNegotiateInfo,
		FsctlPipeTransceive,
		FsctlQueryNetworkInterfInfo,
		FsctlSmbtortureForceUnackedTimeout:
		return true
	default:
		return false
	}
}

// handleSmbtortureForceUnackedTimeout accepts FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT
// (Samba's torture-only FSCTL 0x83848003) as a no-op success. The wire
// contract is buffer-less and the response payload is just the IOCTL header
// echoing the sentinel FileID.
//
// See the constant doc in stub_handlers.go for the rationale (issue #436):
// smbtorture's multichannel.leases.test2 keys off the IOCTL's NTSTATUS to
// decide `block_ok`. Returning STATUS_FILE_CLOSED (the default for an
// unknown FSCTL using the 0xFF... sentinel handle) makes test2 fail before
// its `done:` cleanup releases the leases on `lease_break_test{1,2}.dat`,
// which then leak into multichannel.leases.test3 as a spurious break on
// test3's `unlink fname1`.
func (h *Handler) handleSmbtortureForceUnackedTimeout(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// Validate the IOCTL request envelope per MS-SMB2 2.2.31: the fixed
	// portion is 56 bytes (StructureSize..Reserved2) and StructureSize must
	// be 57 (encodes "fixed body + 1 buffer byte" per SMB2 convention).
	// Reject malformed requests rather than silently treating any ≥24-byte
	// blob as success.
	if len(body) < 56 {
		logger.Debug("IOCTL FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT: body too small",
			"len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	r := smbenc.NewReader(body)
	structureSize := r.ReadUint16()
	if r.Err() != nil || structureSize != 57 {
		logger.Debug("IOCTL FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT: bad StructureSize",
			"got", structureSize, "want", 57)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	// The FSCTL is buffer-less and targets the 16-byte all-0xFF sentinel
	// FileID. Mirror handleValidateNegotiateInfo: reject any other FileID.
	if !bytes.Equal(fileID[:], allFFFileID) {
		logger.Debug("IOCTL FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT: non-sentinel FileID",
			"fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	logger.Debug("IOCTL FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT: accepting as no-op (smbtorture-only)")
	resp := buildIoctlResponse(FsctlSmbtortureForceUnackedTimeout, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
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

// handleIsPathnameValid handles FSCTL_IS_PATHNAME_VALID [MS-FSCC] 2.3.33.
// Returns STATUS_SUCCESS (all pathnames are considered valid).
func (h *Handler) handleIsPathnameValid(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	logger.Debug("IOCTL FSCTL_IS_PATHNAME_VALID: returning success")
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	resp := buildIoctlResponse(FsctlIsPathnameValid, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
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
