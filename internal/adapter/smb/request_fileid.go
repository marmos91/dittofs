package smb

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// ExtractRequestFileID returns the 16-byte FileID carried by the request body
// for SMB2 commands that operate on an open handle. For commands without a
// FileID (NEGOTIATE, SESSION_SETUP, LOGOFF, TREE_CONNECT, TREE_DISCONNECT,
// CREATE, CANCEL, ECHO, OPLOCK_BREAK) it returns false.
//
// Used by the connection dispatcher to pre-acquire the handle's in-flight
// counter BEFORE spawning the request goroutine. Without this, two requests
// on the same TCP connection targeting the same FileID (e.g. QUERY_DIRECTORY
// followed by CLOSE in smbtorture compound_find_close) race at goroutine
// spawn: if the Go runtime schedules CLOSE first, it deletes the OpenFile
// before the prior request has had a chance to call AcquireOpenFile, and
// the prior request fails with STATUS_FILE_CLOSED.
//
// Pre-acquiring on the read loop (sequential, in wire order) makes the
// handleOps counter visible to a later CLOSE regardless of goroutine
// scheduling. Body offsets are fixed per MS-SMB2 §2.2.{15,17,19..22,33..39}.
func ExtractRequestFileID(cmd types.Command, body []byte) ([16]byte, bool) {
	var fileID [16]byte
	off, ok := requestFileIDOffset(cmd)
	if !ok {
		return fileID, false
	}
	if len(body) < off+16 {
		return fileID, false
	}
	copy(fileID[:], body[off:off+16])
	return fileID, true
}

// requestFileIDOffset returns the byte offset of the FileID field in the
// SMB2 request body for the given command, or false if the command has no
// FileID. Offsets are taken from MS-SMB2 §2.2 request structures.
func requestFileIDOffset(cmd types.Command) (int, bool) {
	switch cmd {
	case types.SMB2Close:
		// StructureSize(2) + Flags(2) + Reserved(4)
		return 8, true
	case types.SMB2Flush:
		// StructureSize(2) + Reserved1(2) + Reserved2(4)
		return 8, true
	case types.SMB2Read:
		// StructureSize(2) + Padding(1) + Flags(1) + Length(4) + Offset(8)
		return 16, true
	case types.SMB2Write:
		// StructureSize(2) + DataOffset(2) + Length(4) + Offset(8)
		return 16, true
	case types.SMB2Lock:
		// StructureSize(2) + LockCount(2) + LockSequence(4)
		return 8, true
	case types.SMB2Ioctl:
		// StructureSize(2) + Reserved(2) + CtlCode(4)
		return 8, true
	case types.SMB2QueryDirectory:
		// StructureSize(2) + FileInfoClass(1) + Flags(1) + FileIndex(4)
		return 8, true
	case types.SMB2ChangeNotify:
		// StructureSize(2) + Flags(2) + OutputBufferLength(4)
		return 8, true
	case types.SMB2QueryInfo:
		// StructureSize(2) + InfoType(1) + FileInfoClass(1) + OutputBufferLength(4)
		// + InputBufferOffset(2) + Reserved(2) + InputBufferLength(4)
		// + AdditionalInformation(4) + Flags(4)
		return 24, true
	case types.SMB2SetInfo:
		// StructureSize(2) + InfoType(1) + FileInfoClass(1) + BufferLength(4)
		// + BufferOffset(2) + Reserved(2) + AdditionalInformation(4)
		return 16, true
	}
	return 0, false
}
