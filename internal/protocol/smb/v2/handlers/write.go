package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// WriteRequest represents an SMB2 WRITE request from a client [MS-SMB2] 2.2.21.
//
// The client specifies a FileID, offset, and data to write to a file.
// This structure is decoded from little-endian binary data received over the network.
//
// **Wire Format (49 bytes minimum):**
//
//	Offset  Size  Field                    Description
//	------  ----  -----------------------  ----------------------------------
//	0       2     StructureSize            Always 49
//	2       2     DataOffset               Offset from header to data
//	4       4     Length                   Number of bytes to write
//	8       8     Offset                   File offset to start writing
//	16      16    FileId                   SMB2 file identifier
//	32      4     Channel                  Channel for RDMA (0 = none)
//	36      4     RemainingBytes           Bytes remaining in write (hint)
//	40      2     WriteChannelInfoOffset   Offset to channel info
//	42      2     WriteChannelInfoLength   Length of channel info
//	44      4     Flags                    Write flags
//	48      N     Buffer                   Data to write
//
// **Flags:**
//
//   - SMB2_WRITEFLAG_WRITE_THROUGH (0x00000001): Write-through to disk
//   - SMB2_WRITEFLAG_WRITE_UNBUFFERED (0x00000002): Unbuffered write
//
// **Use Cases:**
//
//   - Sequential file writes (file copies, downloads)
//   - Random access writes (database files)
//   - Large file uploads (client handles chunking)
//   - Creating MFsymlink files (symlinks via SMB)
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
//
// The response indicates how many bytes were actually written.
//
// **Wire Format (17 bytes):**
//
//	Offset  Size  Field                   Description
//	------  ----  ----------------------  ----------------------------------
//	0       2     StructureSize           Always 17
//	2       2     Reserved                Must be ignored
//	4       4     Count                   Number of bytes written
//	8       4     Remaining               Bytes remaining (0 for success)
//	12      2     WriteChannelInfoOffset  Offset to channel info (0)
//	14      2     WriteChannelInfoLength  Length of channel info (0)
//
// **Status Codes:**
//
//   - StatusSuccess: Data written successfully
//   - StatusInvalidHandle: The FileID does not refer to a valid open file
//   - StatusAccessDenied: Write permission denied
//   - StatusDiskFull: Not enough space on disk
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
//
// The decoding extracts all fields including the variable-length data buffer.
// All fields use little-endian byte order per SMB2 specification.
//
// The data buffer location is determined by DataOffset (relative to SMB2 header):
//   - DataOffset - 64 = offset in body
//   - Typical DataOffset is 112 (64 header + 48 fixed part)
//
// **Parameters:**
//   - body: Raw request bytes (49 bytes minimum)
//
// **Returns:**
//   - *WriteRequest: The decoded request containing offset, length, and data
//   - error: ErrRequestTooShort if body is less than 49 bytes
//
// **Example:**
//
//	body := []byte{...} // SMB2 WRITE request from network
//	req, err := DecodeWriteRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter)
//	}
//	// Write req.Data to file at req.Offset
func DecodeWriteRequest(body []byte) (*WriteRequest, error) {
	if len(body) < 49 {
		return nil, fmt.Errorf("WRITE request too short: %d bytes", len(body))
	}

	req := &WriteRequest{
		DataOffset:     binary.LittleEndian.Uint16(body[2:4]),
		Length:         binary.LittleEndian.Uint32(body[4:8]),
		Offset:         binary.LittleEndian.Uint64(body[8:16]),
		Channel:        binary.LittleEndian.Uint32(body[32:36]),
		RemainingBytes: binary.LittleEndian.Uint32(body[36:40]),
		Flags:          binary.LittleEndian.Uint32(body[44:48]),
	}
	copy(req.FileID[:], body[16:32])

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
		} else if len(body) > 49 {
			// Last resort: take whatever data is available after fixed part
			req.Data = body[48:]
		}
	}

	return req, nil
}

// Encode serializes the WriteResponse to SMB2 wire format [MS-SMB2] 2.2.22.
//
// The response indicates the number of bytes successfully written.
//
// **Wire Format (17 bytes):**
//
//	Offset  Size  Field                   Value
//	------  ----  ----------------------  ------
//	0       2     StructureSize           17
//	2       2     Reserved                0
//	4       4     Count                   Bytes written
//	8       4     Remaining               0 (or remaining bytes)
//	12      2     WriteChannelInfoOffset  0
//	14      2     WriteChannelInfoLength  0
//
// **Returns:**
//   - []byte: 17-byte encoded response body
//   - error: Always nil (encoding cannot fail for this structure)
//
// **Example:**
//
//	resp := &WriteResponse{
//	    SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
//	    Count:           65536,
//	    Remaining:       0,
//	}
//	data, _ := resp.Encode()
//	// Send data as response body after SMB2 header
func (resp *WriteResponse) Encode() ([]byte, error) {
	buf := make([]byte, 17)
	binary.LittleEndian.PutUint16(buf[0:2], 17)              // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], 0)               // Reserved
	binary.LittleEndian.PutUint32(buf[4:8], resp.Count)      // Count
	binary.LittleEndian.PutUint32(buf[8:12], resp.Remaining) // Remaining
	binary.LittleEndian.PutUint16(buf[12:14], 0)             // WriteChannelInfoOffset
	binary.LittleEndian.PutUint16(buf[14:16], 0)             // WriteChannelInfoLength

	return buf, nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Write handles SMB2 WRITE command [MS-SMB2] 2.2.21, 2.2.22.
//
// **Purpose:**
//
// WRITE allows clients to write data to an open file at a specified offset.
// SMB2 clients typically write in 32KB chunks (similar to NFS 32KB writes).
//
// **Process:**
//
//  1. Validate FileID maps to an open file (not a directory)
//  2. Get session and tree connection for permission checking
//  3. Verify write permission at share level
//  4. Get metadata and content stores for the share
//  5. Build AuthContext for permission validation
//  6. PrepareWrite - validate permissions and get PayloadID
//  7. Write data to cache (async) or content store (sync)
//  8. CommitWrite - update file metadata (size, timestamps)
//  9. Return success response with bytes written
//
// **Cache Integration:**
//
// Write behavior depends on cache configuration:
//   - With cache (async mode): Writes go to cache first, flushed on FLUSH/CLOSE
//   - Without cache (sync mode): Writes go directly to content store
//
// Async mode is preferred for performance as it allows batching small writes
// and reduces latency. Clients can call FLUSH when durability is required.
//
// **Two-Phase Write Pattern:**
//
// DittoFS uses a two-phase write pattern to maintain consistency:
//   - PrepareWrite: Validates permissions, doesn't modify metadata
//   - WriteAt: Writes data to cache or content store
//   - CommitWrite: Updates metadata (size, mtime) after successful write
//
// This ensures metadata reflects actual data state and provides rollback
// capability if the write fails.
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidHandle: Invalid FileID
//   - StatusInvalidDeviceRequest: Cannot write to directory
//   - StatusUserSessionDeleted: Session no longer valid
//   - StatusAccessDenied: Write permission denied
//   - StatusBadNetworkName: Share not found
//   - StatusUnexpectedIOError: Cache or content store write failed
//   - StatusInternalError: Metadata error
//
// **Performance Considerations:**
//
// WRITE is frequently called and performance-critical:
//   - Uses cache for async writes (reduces latency)
//   - SMB clients typically use 32KB write chunks
//   - PayloadID caching in OpenFile reduces metadata lookups
//   - Parallel writes to different files are supported
//
// **Example:**
//
//	req := &WriteRequest{FileID: fileID, Offset: 0, Data: data}
//	resp, err := handler.Write(ctx, req)
//	if resp.GetStatus() == types.StatusSuccess {
//	    // resp.Count bytes were written
//	}
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
		logger.Debug("WRITE: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// ========================================================================
	// Step 2: Handle named pipe writes (IPC$ RPC)
	// ========================================================================

	if openFile.IsPipe {
		return h.handlePipeWrite(ctx, req, openFile)
	}

	// ========================================================================
	// Step 3: Validate file type
	// ========================================================================

	if openFile.IsDirectory {
		logger.Debug("WRITE: cannot write to directory", "path", openFile.Path)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidDeviceRequest}}, nil
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
	// Step 6: Get metadata and content services
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()
	payloadSvc := h.Registry.GetBlockService()

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
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusLockNotGranted}}, nil
	}

	// ========================================================================
	// Step 9: Prepare write operation
	// ========================================================================

	newSize := req.Offset + uint64(len(req.Data))
	writeOp, err := metaSvc.PrepareWrite(authCtx, openFile.MetadataHandle, newSize)
	if err != nil {
		logger.Debug("WRITE: prepare failed", "path", openFile.Path, "error", err)
		return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// ========================================================================
	// Step 10: Write data to ContentService (uses Cache internally)
	// ========================================================================

	bytesWritten := len(req.Data)

	err = payloadSvc.WriteAt(authCtx.Context, writeOp.PayloadID, req.Data, req.Offset)
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
