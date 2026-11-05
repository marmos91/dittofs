package handlers

import (
	"fmt"

	"github.com/cubbit/dittofs/internal/adapter/common"
	"github.com/cubbit/dittofs/internal/adapter/smb/smbenc"
	"github.com/cubbit/dittofs/internal/adapter/smb/types"
	"github.com/cubbit/dittofs/internal/logger"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// SMB2 WRITE request Flags [MS-SMB2] 2.2.21. Undefined bits are ignored.
const (
	// writeFlagWriteThrough (SMB2_WRITEFLAG_WRITE_THROUGH) asks the server to
	// put the written bytes on stable storage before it answers the request.
	// Not valid for the 2.0.2 dialect, where the bit is ignored.
	writeFlagWriteThrough uint32 = 0x00000001

	// writeFlagWriteUnbuffered (SMB2_WRITEFLAG_WRITE_UNBUFFERED) asks the server
	// not to cache the written bytes at intermediate layers. Not valid for the
	// 2.0.2, 2.1 and 3.0 dialects.
	writeFlagWriteUnbuffered uint32 = 0x00000002
)

// WriteRequest represents an SMB2 WRITE request from a client [MS-SMB2] 2.2.21.
// The client specifies a FileID, offset, and data to write to a file.
// The fixed wire format is exactly 49 bytes, followed by the variable-length data buffer.
type WriteRequest struct {
	// DataOffset is the offset from the start of the SMB2 header
	// to the write data. Typically 64 (header) + 48 (request) = 112.
	DataOffset uint16

	// Length is the number of bytes to write.
	// Maximum is MaxWriteSize from NEGOTIATE response.
	Length uint32

	// Offset is the byte offset in the file to start writing.
	// Zero-based; offset 0 is the first byte of the file.
	Offset uint64

	// FileID is the SMB2 file identifier returned by CREATE.
	// Both persistent (8 bytes) and volatile (8 bytes) parts must match
	// an open file handle on the server.
	FileID [16]byte

	// Channel specifies the RDMA channel (0 for non-RDMA).
	Channel uint32

	// RemainingBytes is a hint about remaining bytes to write (usually 0).
	RemainingBytes uint32

	// Flags controls write behavior; see writeFlagWriteThrough and
	// writeFlagWriteUnbuffered.
	Flags uint32

	// Data contains the bytes to write to the file.
	Data []byte
}

// WriteResponse represents an SMB2 WRITE response [MS-SMB2] 2.2.22.
// The 17-byte response indicates how many bytes were actually written.
type WriteResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method

	// Count is the number of bytes successfully written.
	// Should match the requested length on success.
	Count uint32

	// Remaining indicates bytes remaining to be written.
	// 0 means all data was written successfully.
	Remaining uint32
}

// ============================================================================
// Encoding and Decoding
// ============================================================================

// DecodeWriteRequest parses an SMB2 WRITE request from wire format [MS-SMB2] 2.2.21.
// It extracts all fields including the variable-length data buffer, whose
// location is determined by DataOffset (relative to SMB2 header).
// The fixed structure is 48 bytes (StructureSize=49 includes 1-byte Buffer placeholder).
// Returns an error if the body is less than 48 bytes.
func DecodeWriteRequest(body []byte) (*WriteRequest, error) {
	if len(body) < 48 {
		return nil, fmt.Errorf("WRITE request too short: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	r.Skip(2) // StructureSize
	req := &WriteRequest{
		DataOffset: r.ReadUint16(),
		Length:     r.ReadUint32(),
		Offset:     r.ReadUint64(),
	}
	copy(req.FileID[:], r.ReadBytes(16))
	req.Channel = r.ReadUint32()
	req.RemainingBytes = r.ReadUint32()
	r.Skip(4) // WriteChannelInfoOffset(2) + WriteChannelInfoLength(2)
	req.Flags = r.ReadUint32()
	if r.Err() != nil {
		return nil, fmt.Errorf("WRITE decode error: %w", r.Err())
	}

	// Extract data
	// DataOffset is relative to the beginning of the SMB2 header (64 bytes)
	// Our body starts after the header, so we subtract 64
	// The fixed request structure is 48 bytes (StructureSize says 49 but that includes 1 byte of Buffer)
	// Data typically starts at offset 48 in the body (or wherever DataOffset-64 points)

	if req.Length > 0 {
		// Data must sit exactly where DataOffset says. Clamping a bad offset
		// to 48 would substitute bytes the client did not place in the
		// message — per MS-SMB2 3.3.5.13 the server must fail the request
		// with STATUS_INVALID_PARAMETER instead of writing guessed content.
		dataStart := int(req.DataOffset) - 64
		if dataStart < 48 {
			return nil, fmt.Errorf("write request DataOffset %d points before the fixed structure (body offset %d, minimum 48)",
				req.DataOffset, dataStart)
		}
		// Range first, THEN subtract. Converting both sides to uint64 stops the
		// signed wrap a 32-bit build has on `int(req.Length)`, but it introduces
		// an unsigned one: a DataOffset past the end of the body makes
		// len(body)-dataStart underflow to an enormous number, after which a
		// small Length passes and the slice panics — on every architecture, from
		// a request anyone can send. One bound is not a substitute for the other.
		if dataStart > len(body) {
			return nil, fmt.Errorf("write request DataOffset %d points past the end of the message (body offset %d, have %d)",
				req.DataOffset, dataStart, len(body))
		}
		available := uint64(len(body) - dataStart)
		if uint64(req.Length) > available {
			// "have" is what is left FROM the offset, not the whole body: the
			// body length alone reads as plenty whenever DataOffset is large,
			// which is exactly the case that gets here.
			return nil, fmt.Errorf("write request body too short: need %d bytes at body offset %d, have %d",
				req.Length, dataStart, available)
		}
		req.Data = body[dataStart : dataStart+int(req.Length)]
	}

	return req, nil
}

// Encode serializes the WriteResponse to SMB2 wire format [MS-SMB2] 2.2.22.
// Returns a 17-byte response body.
func (resp *WriteResponse) Encode() ([]byte, error) {
	w := smbenc.NewWriter(17)
	w.WriteUint16(17)             // StructureSize
	w.WriteUint16(0)              // Reserved
	w.WriteUint32(resp.Count)     // Count
	w.WriteUint32(resp.Remaining) // Remaining
	w.WriteUint16(0)              // WriteChannelInfoOffset
	w.WriteUint16(0)              // WriteChannelInfoLength
	if w.Err() != nil {
		return nil, w.Err()
	}
	return w.Bytes(), nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Write handles SMB2 WRITE command [MS-SMB2] 2.2.21, 2.2.22.
//
// WRITE allows clients to write data to an open file at a specified offset.
// It uses a two-phase pattern (PrepareWrite/WriteAt/CommitWrite) to maintain
// consistency between data and metadata. Writes go through the cache layer
// for async performance and are flushed on FLUSH or CLOSE.
//
// The handler validates the file handle, checks share-level write permission,
// verifies no conflicting byte-range locks exist, and returns the number of
// bytes successfully written.
func (h *Handler) Write(ctx *SMBHandlerContext, req *WriteRequest) (*WriteResponse, error) {
	logger.Debug("WRITE request",
		"fileID", lazyFileID(req.FileID),
		"offset", req.Offset,
		"length", req.Length)

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("WRITE: file handle not found (closed)", "fileID", lazyFileID(req.FileID))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileClosed}}, nil
	}

	// ========================================================================
	// Step 2: Handle named pipe writes (IPC$ RPC)
	// ========================================================================

	// Per MS-SMB2 3.3.5.13: Channel selects an RDMA read/write. This transport
	// has no RDMA support, so any nonzero channel cannot be honored — serving
	// the payload inline would bypass the RDMA semantics the client asked for.
	// Fail with STATUS_INVALID_PARAMETER instead of decoding-then-ignoring.
	// Ahead of the pipe dispatch because a named pipe is served by the same
	// transport and can honor RDMA no better than a file can.
	if req.Channel != 0 {
		logger.Debug("WRITE: RDMA channel requested on non-RDMA transport",
			"fileID", lazyFileID(req.FileID), "channel", req.Channel)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	if openFile.IsPipe {
		return h.handlePipeWrite(ctx, req, openFile)
	}

	path := openFile.Name().Path

	// ========================================================================
	// Step 2b: Validate write access
	// ========================================================================
	//
	// Per MS-SMB2 §3.3.5.13 ("Receiving an SMB2 WRITE Request"): the server MUST
	// verify that the open was created
	// with write access (FILE_WRITE_DATA or FILE_APPEND_DATA). If the open
	// lacks write access, return STATUS_ACCESS_DENIED.
	//
	// Gate consults Open.GrantedAccess (post-DACL intersection at CREATE),
	// not the pre-DACL DesiredAccess — otherwise a MAXIMUM_ALLOWED open whose
	// DACL stripped FILE_WRITE_DATA would still pass this check (same fix
	// class as #616 ChangeNotify).

	if !hasWriteAccess(openFile.GrantedAccess) {
		logger.Debug("WRITE: no write access on handle",
			"path", path,
			"grantedAccess", fmt.Sprintf("0x%x", openFile.GrantedAccess))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 3: Validate file type
	// ========================================================================

	if openFile.IsDirectory {
		logger.Debug("WRITE: cannot write to directory", "path", path)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidDeviceRequest}}, nil
	}

	// ========================================================================
	// Step 3b: Validate offset range
	// ========================================================================

	// Per MS-SMB2: offsets with the high bit set (>= 2^63) are invalid.
	// offset + length exceeding INT64_MAX is also invalid.
	// Additionally, writes at or beyond MAXFILESIZE (0xFFFFFFF0000 = 16TiB - 64KiB,
	// the NTFS maximum file size) are rejected.
	// Windows returns STATUS_INVALID_PARAMETER.
	const int64Max = uint64(1<<63 - 1)        // 0x7FFFFFFFFFFFFFFF
	const maxFileSize = uint64(0xFFFFFFF0000) // NTFS max file size (~16TB)
	if req.Offset > int64Max {
		logger.Debug("WRITE: invalid offset (high bit set)", "path", path, "offset", req.Offset)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}
	if len(req.Data) > 0 {
		writeEnd := req.Offset + uint64(len(req.Data))
		if writeEnd < req.Offset || writeEnd > int64Max {
			logger.Debug("WRITE: offset+length overflow or exceeds INT64_MAX", "path", path,
				"offset", req.Offset, "length", len(req.Data))
			return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}
		if writeEnd > maxFileSize {
			logger.Debug("WRITE: offset+length exceeds MAXFILESIZE", "path", path,
				"offset", req.Offset, "length", len(req.Data), "maxFileSize", maxFileSize)
			return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}
		if writeEnd == maxFileSize {
			// Writing right up to MAXFILESIZE boundary: Windows returns DISK_FULL
			logger.Debug("WRITE: at MAXFILESIZE boundary", "path", path,
				"offset", req.Offset, "length", len(req.Data))
			return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusDiskFull}}, nil
		}
	}

	// Per MS-SMB2 3.3.5.13: the server MUST fail a WRITE whose payload exceeds
	// the MaxWriteSize advertised in NEGOTIATE with STATUS_INVALID_PARAMETER —
	// the symmetric contract to the READ MaxReadSize clamp above. MaxWriteSize is
	// non-zero after NEGOTIATE (default 1 MB, handler.go); guard against a zero
	// value so the clamp never rejects every write.
	if h.MaxWriteSize > 0 && uint32(len(req.Data)) > h.MaxWriteSize {
		logger.Debug("WRITE: length exceeds negotiated MaxWriteSize",
			"path", path, "length", len(req.Data), "maxWriteSize", h.MaxWriteSize)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	// ========================================================================
	// Step 4: Get session and tree connection
	// ========================================================================

	if _, ok := h.GetTree(openFile.TreeID); !ok {
		logger.Debug("WRITE: invalid tree ID", "treeID", openFile.TreeID)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}
	if _, ok := h.GetSession(openFile.SessionID); !ok {
		logger.Debug("WRITE: invalid session ID", "sessionID", openFile.SessionID)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusUserSessionDeleted}}, nil
	}
	// Priming also refuses a handle that does not belong to this request's
	// TreeConnect/Session (MS-SMB2 §3.3.5.2.5).
	if status := h.primeAuthContextFromOpenFile(ctx, openFile); status != types.StatusSuccess {
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: status}}, nil
	}

	// ========================================================================
	// Step 5: Check write permission at share level
	// ========================================================================

	if !HasWritePermission(ctx) {
		logger.Debug("WRITE: access denied", "path", path, "permission", ctx.Permission)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 5b: Handle zero-length writes
	// ========================================================================

	// Per MS-SMB2: zero-length writes are valid no-ops that return success
	// with 0 bytes written. Skip the prepare/write/commit cycle.
	if len(req.Data) == 0 {
		logger.Debug("WRITE: zero-length write (no-op)", "path", path, "offset", req.Offset)
		// A no-op for the data, but still a successful WRITE, so MS-FSA 2.1.5.4
		// applies the same way it does below: CurrentByteOffset becomes
		// ByteOffset + BytesWritten, which here is the offset itself. Leaving it
		// alone would report the position of the write before this one.
		recordReadProgress(openFile, req.Offset, 0)
		return &WriteResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Count:           0,
			Remaining:       0,
		}, nil
	}

	// ========================================================================
	// Step 6: Get metadata service
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()

	// ========================================================================
	// Step 7: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("WRITE: failed to build auth context", "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// SMB write authorization is HANDLE-BASED: the open already enforced the
	// DACL ceiling (Step 2b verified the handle carries FILE_WRITE_DATA /
	// FILE_APPEND_DATA via OpenFile.GrantedAccess). Signal the metadata layer
	// not to re-deny this WRITE on the file's POSIX mode / DOS-READONLY
	// attribute (smbtorture smb2.durable-open.read-only writes through a handle
	// opened on a FILE_ATTRIBUTE_READONLY file). An explicit DENY-write ACE
	// would have stripped write at open, so this flag can only be set when the
	// open legitimately granted write.
	authCtx.WriteAuthorizedByHandle = hasWriteAccess(openFile.GrantedAccess)

	// ========================================================================
	// Step 8: Check for conflicting byte-range locks
	// ========================================================================

	// Writes are blocked by any other session's lock (shared or exclusive)
	if err := metaSvc.CheckLockForIO(
		authCtx.Context,
		openFile.MetadataHandle,
		openFile.OpenID(),
		ctx.SessionID,
		req.Offset,
		uint64(len(req.Data)),
		true, // isWrite = true for write operations
	); err != nil {
		logger.Debug("WRITE: blocked by lock", "path", path, "offset", req.Offset, "length", len(req.Data))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileLockConflict}}, nil
	}

	// ========================================================================
	// Step 9: Validate the write, then answer
	// ========================================================================
	//
	// PrepareWrite validates the target and modifies no metadata, so a refused
	// write still answers with its own status. A write that would go through
	// stores content, and no content store is wired: it answers before any
	// lease is broken or durable handle purged, since nothing changes.
	if _, err := metaSvc.PrepareWrite(authCtx, openFile.MetadataHandle, req.Offset+uint64(len(req.Data))); err != nil {
		logger.Debug("WRITE: prepare failed", "path", path, "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(err)}}, nil
	}
	return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(common.ErrNoContentStore)}}, nil
}

// handlePipeWrite handles WRITE to a named pipe for DCE/RPC communication.
func (h *Handler) handlePipeWrite(ctx *SMBHandlerContext, req *WriteRequest, openFile *OpenFile) (*WriteResponse, error) {
	logger.Debug("WRITE to named pipe",
		"pipeName", openFile.PipeName,
		"dataLen", len(req.Data))

	// Per MS-SMB2 3.3.5.13, a server that implements the SMB 3.x dialect family
	// MUST reject WRITE_THROUGH/WRITE_UNBUFFERED on a named pipe with
	// STATUS_INVALID_PARAMETER. The condition is on what the server implements,
	// not on the dialect this connection negotiated, so it keys off the
	// configured maximum dialect; a server capped below 3.0 keeps ignoring the
	// bits as the 2.x dialects require.
	if h.MaxDialect >= types.Dialect0300 && req.Flags&(writeFlagWriteThrough|writeFlagWriteUnbuffered) != 0 {
		logger.Debug("WRITE: write-through/unbuffered flags are invalid on a named pipe",
			"pipeName", openFile.PipeName,
			"flags", req.Flags)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	pipe := h.PipeManager.GetPipe(req.FileID)
	if pipe == nil {
		logger.Debug("WRITE: pipe not found", "fileID", lazyFileID(req.FileID))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	err := pipe.ProcessWrite(req.Data)
	if err != nil {
		logger.Warn("WRITE: pipe write failed", "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
	}

	// Complete any pending async READ for this pipe handle.
	// The WRITE produced response data in ReadBuffer; deliver it now.
	if h.PipeReadRegistry != nil {
		if pending := h.PipeReadRegistry.UnregisterByFileID(req.FileID); pending != nil {
			data := pipe.ProcessRead(pending.MaxLen)
			if pending.Callback != nil {
				go func() {
					if err := pending.Callback(pending.SessionID, pending.MessageID, pending.AsyncId, types.StatusSuccess, data); err != nil {
						logger.Warn("WRITE: failed to complete pending pipe READ",
							"asyncId", pending.AsyncId,
							"error", err)
					}
				}()
			}
		}
	}

	logger.Debug("WRITE to pipe successful",
		"pipeName", openFile.PipeName,
		"bytesWritten", len(req.Data))

	return &WriteResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		Count:           uint32(len(req.Data)),
		Remaining:       0,
	}, nil
}
