package smb

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// channelSeqModify reports whether a command is a modifying operation for the
// purposes of SMB3 channel-sequence verification (MS-SMB2 §3.3.5.2.10). Only
// WRITE, SET_INFO, and IOCTL are rejected on a stale ChannelSequence; all
// other commands are read-only with respect to this check (Samba marks the
// same three with .modify = true in source3/smbd/smb2_server.c).
func channelSeqModify(cmd types.Command) bool {
	switch cmd {
	case types.CommandWrite, types.CommandSetInfo, types.CommandIoctl:
		return true
	default:
		return false
	}
}

// channelSeqFileID extracts the 16-byte FileId from a request body for the
// handle-scoped commands that participate in channel-sequence verification.
// The body slice excludes the 64-byte SMB2 header. Returns ok=false for
// commands that do not carry a FileId at a fixed offset (the check is skipped
// for those). FileId offsets per MS-SMB2: READ §2.2.19 / WRITE §2.2.21 /
// SET_INFO §2.2.39 all place it 16 bytes into the body; IOCTL §2.2.31 places
// it 8 bytes in (after StructureSize, Reserved, CtlCode).
func channelSeqFileID(cmd types.Command, body []byte) (fileID [16]byte, ok bool) {
	var off int
	switch cmd {
	case types.CommandRead, types.CommandWrite, types.CommandSetInfo:
		off = 16
	case types.CommandIoctl:
		off = 8
	default:
		return fileID, false
	}
	if len(body) < off+16 {
		return fileID, false
	}
	copy(fileID[:], body[off:off+16])
	return fileID, true
}

// verifyChannelSequence runs the SMB3 channel-sequence check (MS-SMB2
// §3.3.5.2.10) for a handle-scoped request. It returns the status the dispatch
// path should reject with (StatusFileNotAvailable) when a modifying operation
// arrives on a stale ChannelSequence, or 0 when the request may proceed.
//
// The check is a no-op for non-SMB3 connections, for commands without a
// FileId, and when the target Open is unknown (handle-not-found is left to the
// handler to report with its own status). Read-only commands never reject but
// still advance the tracked sequence so a following modifying op observes the
// up-to-date value.
func verifyChannelSequence(reqHeader *header.SMB2Header, body []byte, connInfo *ConnInfo) types.Status {
	if connInfo.CryptoState == nil || connInfo.CryptoState.GetDialect() < types.Dialect0300 {
		return 0
	}

	fileID, ok := channelSeqFileID(reqHeader.Command, body)
	if !ok {
		return 0
	}

	openFile, ok := connInfo.Handler.GetOpenFile(fileID)
	if !ok {
		return 0
	}

	// The ChannelSequence occupies the low 16 bits of the 4-byte
	// ChannelSequence/Reserved field, which the header parser surfaces as
	// Status in requests (MS-SMB2 §2.2.1.2).
	reqCSN := uint16(uint32(reqHeader.Status) & 0xFFFF)

	if openFile.VerifyChannelSequence(reqCSN, channelSeqModify(reqHeader.Command)) {
		return 0
	}
	return types.StatusFileNotAvailable
}
