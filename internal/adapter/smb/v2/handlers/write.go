package handlers

import (
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

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

	// Flags controls write behavior.
	// Common values:
	//   - 0x00000000: Normal buffered write
	//   - 0x00000001: Write-through (bypass server cache)
	//   - 0x00000002: Unbuffered write
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
		// Calculate where data starts in body
		dataStart := int(req.DataOffset) - 64

		// Clamp to valid range - data can't start before byte 48 (after fixed fields)
		if dataStart < 48 {
			dataStart = 48
		}

		// Try to extract data from calculated offset
		if dataStart+int(req.Length) <= len(body) {
			req.Data = body[dataStart : dataStart+int(req.Length)]
		} else if len(body) > 48 && int(req.Length) <= len(body)-48 {
			// Fallback: data might be right after the 48-byte fixed structure
			req.Data = body[48 : 48+int(req.Length)]
		} else {
			return nil, fmt.Errorf("write request body too short: need %d bytes, have %d", req.Length, len(body)-48)
		}
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
		"fileID", fmt.Sprintf("%x", req.FileID),
		"offset", req.Offset,
		"length", req.Length)

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("WRITE: file handle not found (closed)", "fileID", fmt.Sprintf("%x", req.FileID))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileClosed}}, nil
	}

	// ========================================================================
	// Step 2: Handle named pipe writes (IPC$ RPC)
	// ========================================================================

	if openFile.IsPipe {
		return h.handlePipeWrite(ctx, req, openFile)
	}

	// ========================================================================
	// Step 2b: Validate write access
	// ========================================================================
	//
	// Per MS-SMB2 3.3.5.16: The server MUST verify that the open was created
	// with write access (FILE_WRITE_DATA or FILE_APPEND_DATA). If the open
	// lacks write access, return STATUS_ACCESS_DENIED.

	if !hasWriteAccess(openFile.DesiredAccess) {
		logger.Debug("WRITE: no write access on handle",
			"path", openFile.Path,
			"desiredAccess", fmt.Sprintf("0x%x", openFile.DesiredAccess))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 3: Validate file type
	// ========================================================================

	if openFile.IsDirectory {
		logger.Debug("WRITE: cannot write to directory", "path", openFile.Path)
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
		logger.Debug("WRITE: invalid offset (high bit set)", "path", openFile.Path, "offset", req.Offset)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}
	if len(req.Data) > 0 && req.Offset > int64Max-uint64(len(req.Data)) {
		logger.Debug("WRITE: offset+length exceeds INT64_MAX", "path", openFile.Path,
			"offset", req.Offset, "length", len(req.Data))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}
	if len(req.Data) > 0 {
		writeEnd := req.Offset + uint64(len(req.Data))
		if writeEnd > maxFileSize {
			logger.Debug("WRITE: offset+length exceeds MAXFILESIZE", "path", openFile.Path,
				"offset", req.Offset, "length", len(req.Data), "maxFileSize", maxFileSize)
			return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}
		if writeEnd == maxFileSize {
			// Writing right up to MAXFILESIZE boundary: Windows returns DISK_FULL
			logger.Debug("WRITE: at MAXFILESIZE boundary", "path", openFile.Path,
				"offset", req.Offset, "length", len(req.Data))
			return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusDiskFull}}, nil
		}
	}

	// ========================================================================
	// Step 4: Get session and tree connection
	// ========================================================================

	tree, ok := h.GetTree(openFile.TreeID)
	if !ok {
		logger.Debug("WRITE: invalid tree ID", "treeID", openFile.TreeID)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	sess, ok := h.GetSession(openFile.SessionID)
	if !ok {
		logger.Debug("WRITE: invalid session ID", "sessionID", openFile.SessionID)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusUserSessionDeleted}}, nil
	}

	// Update context
	ctx.ShareName = tree.ShareName
	ctx.User = sess.User
	ctx.IsGuest = sess.IsGuest
	ctx.Permission = tree.Permission

	// ========================================================================
	// Step 5: Check write permission at share level
	// ========================================================================

	if !HasWritePermission(ctx) {
		logger.Debug("WRITE: access denied", "path", openFile.Path, "permission", ctx.Permission)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 5b: Handle zero-length writes
	// ========================================================================

	// Per MS-SMB2: zero-length writes are valid no-ops that return success
	// with 0 bytes written. Skip the prepare/write/commit cycle.
	if len(req.Data) == 0 {
		logger.Debug("WRITE: zero-length write (no-op)", "path", openFile.Path, "offset", req.Offset)
		return &WriteResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Count:           0,
			Remaining:       0,
		}, nil
	}

	// ========================================================================
	// Step 6: Get metadata service and block store
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()
	blockStore, err := h.Registry.GetBlockStoreForHandle(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Warn("WRITE: block store not available for handle", "path", openFile.Path, "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
	}

	// ========================================================================
	// Step 7: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("WRITE: failed to build auth context", "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 8: Check for conflicting byte-range locks
	// ========================================================================

	// Writes are blocked by any other session's lock (shared or exclusive)
	if err := metaSvc.CheckLockForIO(
		authCtx.Context,
		openFile.MetadataHandle,
		ctx.SessionID,
		req.Offset,
		uint64(len(req.Data)),
		true, // isWrite = true for write operations
	); err != nil {
		logger.Debug("WRITE: blocked by lock", "path", openFile.Path, "offset", req.Offset, "length", len(req.Data))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileLockConflict}}, nil
	}

	// ========================================================================
	// Step 9: Prepare write operation
	// ========================================================================

	// Validate offset + length doesn't overflow
	writeLen := uint64(len(req.Data))
	newSize := req.Offset + writeLen
	if newSize < req.Offset {
		// Overflow - invalid parameter, not a disk space issue
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}
	writeOp, err := metaSvc.PrepareWrite(authCtx, openFile.MetadataHandle, newSize)
	if err != nil {
		logger.Debug("WRITE: prepare failed", "path", openFile.Path, "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// ========================================================================
	// Step 10: Write data to BlockStore (uses local cache internally)
	// ========================================================================

	bytesWritten := len(req.Data)

	err = blockStore.WriteAt(authCtx.Context, string(writeOp.PayloadID), req.Data, req.Offset)
	if err != nil {
		logger.Warn("WRITE: content write failed", "path", openFile.Path, "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: ContentErrorToSMBStatus(err)}}, nil
	}

	// ========================================================================
	// Step 11: Commit write operation
	// ========================================================================

	_, err = metaSvc.CommitWrite(authCtx, writeOp)
	if err != nil {
		logger.Warn("WRITE: commit failed", "path", openFile.Path, "error", err)
		// Data was written but metadata not updated - this is an inconsistent state
		// but we still report the error
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// SMB requires immediate metadata visibility across sessions (unlike NFS
	// which has explicit COMMIT). Flush deferred metadata so that other sessions
	// reading the same file see updated size/timestamps without a FLUSH command.
	if _, flushErr := metaSvc.FlushPendingWriteForFile(authCtx, openFile.MetadataHandle); flushErr != nil {
		logger.Debug("WRITE: deferred metadata flush failed (non-fatal)", "path", openFile.Path, "error", flushErr)
	}

	// Per MS-FSA 2.1.5.14.2: If timestamps are frozen via SET_INFO with -1,
	// CommitWrite unconditionally updated Mtime/Ctime. Restore frozen values.
	h.restoreFrozenTimestamps(authCtx, openFile)

	// Per NTFS: Writing to an ADS updates the base object's ChangeTime and
	// LastWriteTime. The ADS is an attribute of the base file/directory, so
	// data writes to the stream propagate timestamp changes to the base
	// object. Respect frozen state on the ADS handle: if a timestamp is
	// frozen, the base object's corresponding timestamp remains unchanged.
	if colonIdx := strings.Index(openFile.FileName, ":"); colonIdx > 0 {
		h.updateBaseObjectTimestampsForADSWrite(authCtx, metaSvc, openFile, openFile.FileName[:colonIdx])
	}

	// Per MS-FSA 2.1.5.3: After a successful write, update LastAccessTime
	// to the current system time, unless frozen via SET_INFO -1.
	if !openFile.AtimeFrozen {
		now := time.Now()
		_ = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, &metadata.SetAttrs{Atime: &now})
	}

	// Update cached PayloadID in OpenFile
	openFile.PayloadID = writeOp.PayloadID

	logger.Debug("WRITE successful",
		"path", openFile.Path,
		"offset", req.Offset,
		"bytes", bytesWritten)

	// ========================================================================
	// Step 12: Return success response
	// ========================================================================

	return &WriteResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		Count:           uint32(bytesWritten),
		Remaining:       0,
	}, nil
}

// handlePipeWrite handles WRITE to a named pipe for DCE/RPC communication.
func (h *Handler) handlePipeWrite(ctx *SMBHandlerContext, req *WriteRequest, openFile *OpenFile) (*WriteResponse, error) {
	logger.Debug("WRITE to named pipe",
		"pipeName", openFile.PipeName,
		"dataLen", len(req.Data))

	// Get pipe state
	pipe := h.PipeManager.GetPipe(req.FileID)
	if pipe == nil {
		logger.Warn("WRITE: pipe not found", "fileID", fmt.Sprintf("%x", req.FileID))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// Process RPC data
	err := pipe.ProcessWrite(req.Data)
	if err != nil {
		logger.Warn("WRITE: pipe write failed", "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
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
