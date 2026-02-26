package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// ReadRequest represents an SMB2 READ request from a client [MS-SMB2] 2.2.19.
// The client specifies a FileID, offset, and length to read from a file.
// The fixed wire format is 49 bytes minimum.
type ReadRequest struct {
	// Padding is an alignment byte (ignored by server).
	Padding uint8

	// Flags controls read behavior (SMB 3.x only).
	// Common values:
	//   - 0x00: Normal read
	//   - 0x01: UNBUFFERED (bypass server cache)
	Flags uint8

	// Length is the number of bytes the client wants to read.
	// Maximum is MaxReadSize from NEGOTIATE response (typically 1MB-64MB).
	Length uint32

	// Offset is the byte offset in the file to start reading from.
	// Zero-based; offset 0 is the first byte of the file.
	Offset uint64

	// FileID is the SMB2 file identifier returned by CREATE.
	// Both persistent (8 bytes) and volatile (8 bytes) parts must match
	// an open file handle on the server.
	FileID [16]byte

	// MinimumCount is the minimum bytes the server must return.
	// 0 means same as Length. Used for network optimization.
	MinimumCount uint32

	// Channel specifies the RDMA channel (0 for non-RDMA).
	Channel uint32

	// RemainingBytes is a hint about remaining file size (usually 0).
	RemainingBytes uint32
}

// ReadResponse represents an SMB2 READ response [MS-SMB2] 2.2.20.
// The response contains the data read from the file. The header is 16 bytes
// followed by the variable-length data buffer.
type ReadResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method

	// DataOffset is the offset from the start of the SMB2 header
	// to the beginning of the Data buffer. Standard value is 0x50 (80).
	DataOffset uint8

	// Data contains the bytes read from the file.
	// Length may be less than requested if approaching EOF.
	Data []byte

	// DataRemaining indicates bytes remaining to be read.
	// 0 means this is the last chunk of the read operation.
	DataRemaining uint32
}

// ============================================================================
// Encoding and Decoding
// ============================================================================

// DecodeReadRequest parses an SMB2 READ request from wire format [MS-SMB2] 2.2.19.
// Returns an error if the body is less than 49 bytes.
func DecodeReadRequest(body []byte) (*ReadRequest, error) {
	if len(body) < 49 {
		return nil, fmt.Errorf("READ request too short: %d bytes", len(body))
	}

	req := &ReadRequest{
		Padding:        body[2],
		Flags:          body[3],
		Length:         binary.LittleEndian.Uint32(body[4:8]),
		Offset:         binary.LittleEndian.Uint64(body[8:16]),
		MinimumCount:   binary.LittleEndian.Uint32(body[32:36]),
		Channel:        binary.LittleEndian.Uint32(body[36:40]),
		RemainingBytes: binary.LittleEndian.Uint32(body[40:44]),
	}
	copy(req.FileID[:], body[16:32])

	return req, nil
}

// Encode serializes the ReadResponse to SMB2 wire format [MS-SMB2] 2.2.20.
// The response header is 16 bytes followed by the data buffer.
func (resp *ReadResponse) Encode() ([]byte, error) {
	// Response header is 16 bytes, data follows at offset 16
	buf := make([]byte, 16+len(resp.Data))
	binary.LittleEndian.PutUint16(buf[0:2], 17)                     // StructureSize (17 per spec)
	buf[2] = resp.DataOffset                                        // DataOffset (relative to header start)
	buf[3] = 0                                                      // Reserved
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(resp.Data))) // DataLength
	binary.LittleEndian.PutUint32(buf[8:12], resp.DataRemaining)    // DataRemaining
	binary.LittleEndian.PutUint32(buf[12:16], 0)                    // Reserved2
	copy(buf[16:], resp.Data)                                       // Buffer starts at offset 16

	return buf, nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Read handles SMB2 READ command [MS-SMB2] 2.2.19, 2.2.20.
//
// READ allows clients to read data from an open file at a specified offset.
// It validates the file handle and permissions, handles symlink reads
// (generating MFsymlink content on-the-fly), checks for conflicting
// byte-range locks, and reads data from the content service (which uses
// the cache layer internally). Returns StatusEndOfFile when the offset
// is at or beyond EOF.
func (h *Handler) Read(ctx *SMBHandlerContext, req *ReadRequest) (*ReadResponse, error) {
	logger.Debug("READ request",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"offset", req.Offset,
		"length", req.Length)

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("READ: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// ========================================================================
	// Step 2: Handle named pipe reads (IPC$ RPC)
	// ========================================================================

	if openFile.IsPipe {
		return h.handlePipeRead(ctx, req, openFile)
	}

	// ========================================================================
	// Step 3: Validate file type
	// ========================================================================

	if openFile.IsDirectory {
		logger.Debug("READ: cannot read from directory", "path", openFile.Path)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidDeviceRequest}}, nil
	}

	// ========================================================================
	// Step 4: Get session and tree connection
	// ========================================================================

	tree, ok := h.GetTree(openFile.TreeID)
	if !ok {
		logger.Debug("READ: invalid tree ID", "treeID", openFile.TreeID)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	sess, ok := h.GetSession(openFile.SessionID)
	if !ok {
		logger.Debug("READ: invalid session ID", "sessionID", openFile.SessionID)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusUserSessionDeleted}}, nil
	}

	// Update context
	ctx.ShareName = tree.ShareName
	ctx.User = sess.User
	ctx.IsGuest = sess.IsGuest
	ctx.Permission = tree.Permission

	// ========================================================================
	// Step 5: Get metadata and content services
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()
	payloadSvc := h.Registry.GetBlockService()

	// ========================================================================
	// Step 6: Build AuthContext and validate permissions
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("READ: failed to build auth context", "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 7: Check for symlink - generate MFsymlink content on-the-fly
	// ========================================================================

	file, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("READ: failed to get file metadata", "path", openFile.Path, "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// Handle symlink reads - SMB clients expect MFsymlink content for symlinks
	if file.Type == metadata.FileTypeSymlink {
		return h.handleSymlinkRead(ctx, openFile, file, req)
	}

	// Validate read permission using PrepareRead (for regular files only)
	readMeta, err := metaSvc.PrepareRead(authCtx, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("READ: permission check failed", "path", openFile.Path, "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// ========================================================================
	// Step 8: Check for conflicting byte-range locks
	// ========================================================================

	// Reads are blocked by another session's exclusive locks
	if err := metaSvc.CheckLockForIO(
		authCtx.Context,
		openFile.MetadataHandle,
		ctx.SessionID,
		req.Offset,
		uint64(req.Length),
		false, // isWrite = false for read operations
	); err != nil {
		logger.Debug("READ: blocked by lock", "path", openFile.Path, "offset", req.Offset, "length", req.Length)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusLockNotGranted}}, nil
	}

	// ========================================================================
	// Step 9: Handle empty file or offset beyond EOF
	// ========================================================================

	fileSize := readMeta.Attr.Size

	if readMeta.Attr.PayloadID == "" || fileSize == 0 {
		logger.Debug("READ: empty file", "path", openFile.Path)
		return &ReadResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			DataOffset:      0x50, // Standard offset
			Data:            []byte{},
			DataRemaining:   0,
		}, nil
	}

	if req.Offset >= fileSize {
		logger.Debug("READ: offset beyond EOF", "path", openFile.Path, "offset", req.Offset, "size", fileSize)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusEndOfFile}}, nil
	}

	// ========================================================================
	// Step 10: Calculate read range
	// ========================================================================

	readEnd := req.Offset + uint64(req.Length)
	if readEnd > fileSize {
		readEnd = fileSize
	}
	actualLength := uint32(readEnd - req.Offset)

	// ========================================================================
	// Step 11: Read data from ContentService (uses Cache internally)
	// ========================================================================

	data := make([]byte, actualLength)
	n, err := payloadSvc.ReadAt(authCtx.Context, readMeta.Attr.PayloadID, data, req.Offset)
	if err != nil {
		logger.Warn("READ: content read failed", "path", openFile.Path, "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: ContentErrorToSMBStatus(err)}}, nil
	}
	data = data[:n]

	logger.Debug("READ successful",
		"path", openFile.Path,
		"offset", req.Offset,
		"requested", req.Length,
		"actual", len(data))

	// ========================================================================
	// Step 12: Return success response
	// ========================================================================

	return &ReadResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		DataOffset:      0x50, // Standard offset (header + response struct)
		Data:            data,
		DataRemaining:   0,
	}, nil
}

// handleSymlinkRead generates MFsymlink content for a symlink read request.
// SMB clients (macOS, Windows) expect symlinks to be stored as MFsymlink files -
// regular files with a special XSym format containing the symlink target.
// This function generates that content on-the-fly from the symlink's LinkTarget
// and returns the appropriate portion based on the request offset and length.
func (h *Handler) handleSymlinkRead(
	ctx *SMBHandlerContext,
	openFile *OpenFile,
	file *metadata.File,
	req *ReadRequest,
) (*ReadResponse, error) {
	// Generate MFsymlink content from the symlink target
	mfsymlinkData, err := mfsymlink.Encode(file.LinkTarget)
	if err != nil {
		logger.Warn("READ: failed to encode MFsymlink",
			"path", openFile.Path,
			"target", file.LinkTarget,
			"error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
	}

	fileSize := uint64(len(mfsymlinkData)) // Always 1067 bytes

	// Handle offset beyond EOF
	if req.Offset >= fileSize {
		logger.Debug("READ: symlink offset beyond EOF",
			"path", openFile.Path,
			"offset", req.Offset,
			"size", fileSize)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusEndOfFile}}, nil
	}

	// Calculate read range
	readEnd := req.Offset + uint64(req.Length)
	if readEnd > fileSize {
		readEnd = fileSize
	}

	// Extract the requested portion
	data := mfsymlinkData[req.Offset:readEnd]

	logger.Debug("READ: symlink (MFsymlink)",
		"path", openFile.Path,
		"target", file.LinkTarget,
		"offset", req.Offset,
		"requested", req.Length,
		"actual", len(data))

	// Build response
	return &ReadResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		DataOffset:      0x50, // Standard offset
		Data:            data,
		DataRemaining:   0,
	}, nil
}

// handlePipeRead handles READ from a named pipe for DCE/RPC communication.
func (h *Handler) handlePipeRead(ctx *SMBHandlerContext, req *ReadRequest, openFile *OpenFile) (*ReadResponse, error) {
	logger.Debug("READ from named pipe",
		"pipeName", openFile.PipeName,
		"requestedLength", req.Length)

	// Get pipe state
	pipe := h.PipeManager.GetPipe(req.FileID)
	if pipe == nil {
		logger.Warn("READ: pipe not found", "fileID", fmt.Sprintf("%x", req.FileID))
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// Read buffered RPC response
	data := pipe.ProcessRead(int(req.Length))
	if len(data) == 0 {
		// No data available - this could be normal if WRITE hasn't happened yet
		logger.Debug("READ: no data available in pipe", "pipeName", openFile.PipeName)
		return &ReadResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			DataOffset:      0x50,
			Data:            []byte{},
			DataRemaining:   0,
		}, nil
	}

	logger.Debug("READ from pipe successful",
		"pipeName", openFile.PipeName,
		"bytesRead", len(data))

	return &ReadResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		DataOffset:      0x50,
		Data:            data,
		DataRemaining:   0,
	}, nil
}
