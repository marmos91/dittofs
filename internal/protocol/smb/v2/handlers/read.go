package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// seekBufferSize is the buffer size used when skipping bytes in a reader
// that doesn't support seeking. This value balances memory usage against
// the number of syscalls needed to skip large offsets. 8KB is chosen as
// a reasonable trade-off for typical file read patterns.
const seekBufferSize = 8192

// ============================================================================
// Request and Response Structures
// ============================================================================

// ReadRequest represents an SMB2 READ request from a client [MS-SMB2] 2.2.19.
//
// The client specifies a FileID, offset, and length to read from a file.
// This structure is decoded from little-endian binary data received over the network.
//
// **Wire Format (49 bytes minimum):**
//
//	Offset  Size  Field               Description
//	------  ----  ------------------  ----------------------------------
//	0       2     StructureSize       Always 49
//	2       1     Padding             Padding byte
//	3       1     Flags               Read flags (SMB 3.x)
//	4       4     Length              Number of bytes to read
//	8       8     Offset              File offset to start reading
//	16      16    FileId              SMB2 file identifier
//	32      4     MinimumCount        Minimum bytes to read (0 = Length)
//	36      4     Channel             Channel for RDMA (0 = none)
//	40      4     RemainingBytes      Bytes remaining in file (hint)
//	44      2     ReadChannelInfoOffset
//	46      2     ReadChannelInfoLength
//	48      1     Buffer              Variable padding
//
// **Use Cases:**
//
//   - Sequential file reads (streaming media, file copies)
//   - Random access reads (database files, memory-mapped files)
//   - Large file streaming (client handles chunking)
//   - MFsymlink reads (symlinks appear as 1067-byte files)
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
//
// The response contains the data read from the file along with metadata
// about the read operation.
//
// **Wire Format (16 bytes header + variable data):**
//
//	Offset  Size  Field           Description
//	------  ----  --------------  ----------------------------------
//	0       2     StructureSize   Always 17 (includes 1 byte of buffer)
//	2       1     DataOffset      Offset to data from header start
//	3       1     Reserved        Must be ignored
//	4       4     DataLength      Number of bytes in Buffer
//	8       4     DataRemaining   Bytes remaining (0 for last chunk)
//	12      4     Reserved2       Must be ignored
//	16      N     Buffer          File data
//
// **Status Codes:**
//
//   - StatusSuccess: Data read successfully
//   - StatusEndOfFile: Requested offset is at or beyond EOF
//   - StatusInvalidHandle: The FileID does not refer to a valid open file
//   - StatusAccessDenied: Read permission denied
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
//
// The decoding extracts all relevant fields from the binary request body.
// All fields use little-endian byte order per SMB2 specification.
//
// **Parameters:**
//   - body: Raw request bytes (49 bytes minimum)
//
// **Returns:**
//   - *ReadRequest: The decoded request containing file location and size
//   - error: ErrRequestTooShort if body is less than 49 bytes
//
// **Example:**
//
//	body := []byte{...} // SMB2 READ request from network
//	req, err := DecodeReadRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter)
//	}
//	// Read req.Length bytes from file at req.Offset
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
//
// The response header is 16 bytes, followed by the data buffer.
// The DataOffset field specifies where the data starts relative to
// the SMB2 header (typically 0x50 = 80 bytes = 64 header + 16 response).
//
// **Wire Format:**
//
//	Offset  Size  Field           Value
//	------  ----  --------------  ------
//	0       2     StructureSize   17 (per spec, includes 1 byte of buffer)
//	2       1     DataOffset      Offset from header start to data
//	3       1     Reserved        0
//	4       4     DataLength      len(Data)
//	8       4     DataRemaining   Remaining bytes hint
//	12      4     Reserved2       0
//	16      N     Buffer          File data
//
// **Returns:**
//   - []byte: Encoded response body (16 bytes + data)
//   - error: Always nil (encoding cannot fail for this structure)
//
// **Example:**
//
//	resp := &ReadResponse{
//	    SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
//	    DataOffset:      0x50,
//	    Data:            fileData,
//	    DataRemaining:   0,
//	}
//	data, _ := resp.Encode()
//	// Send data as response body after SMB2 header
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
// **Purpose:**
//
// READ allows clients to read data from an open file at a specified offset.
// This is one of the most frequently called SMB2 operations.
//
// **Process:**
//
//  1. Validate FileID maps to an open file (not a directory)
//  2. Get session and tree connection for context
//  3. Get metadata and content stores for the share
//  4. Build AuthContext and validate read permission via PrepareRead
//  5. Handle symlink reads (generate MFsymlink content on-the-fly)
//  6. Handle empty file or offset beyond EOF
//  7. Calculate actual read range (may be truncated at EOF)
//  8. Read data from cache or content store
//  9. Return success response with data
//
// **Cache Integration:**
//
// READ uses a read-through cache for optimal performance:
//   - Cache hit (dirty data): Reads from cache for files being written
//   - Cache hit (clean data): Reads from cache for recently accessed files
//   - Cache miss: Reads from content store
//
// Cache state handling:
//   - StateBuffering/StateUploading: Must read from cache (content store may not have data)
//   - StateCached: Read from cache if metadata validation passes
//   - StatePrefetching/StateNone: Read from content store
//
// **Content Store Integration:**
//
// For cache misses, READ prefers efficient partial reads:
//   - ReadAtContentStore interface: Uses ReadAt for efficient random access
//   - Basic ContentStore: Falls back to ReadContent + seek (less efficient)
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidHandle: Invalid FileID
//   - StatusInvalidDeviceRequest: Cannot read from directory
//   - StatusUserSessionDeleted: Session no longer valid
//   - StatusAccessDenied: Read permission denied
//   - StatusBadNetworkName: Share not found
//   - StatusEndOfFile: Offset beyond file size
//   - StatusInternalError: Content read or encoding error
//
// **Performance Considerations:**
//
// READ is frequently called and performance-critical:
//   - Uses ReadAt interface for efficient partial reads
//   - Avoids reading entire file when only a portion is needed
//   - SMB clients typically request 32KB-64KB chunks
//   - Parallel reads from different clients are supported
//
// **Example:**
//
//	req := &ReadRequest{FileID: fileID, Offset: 0, Length: 65536}
//	resp, err := handler.Read(ctx, req)
//	if resp.GetStatus() == types.StatusSuccess {
//	    // Use resp.Data
//	}
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
	// Step 4: Get metadata and content stores
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(tree.ShareName)
	if err != nil {
		logger.Warn("READ: failed to get metadata store", "share", tree.ShareName, "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusBadNetworkName}}, nil
	}

	contentStore, err := h.Registry.GetContentStoreForShare(tree.ShareName)
	if err != nil {
		logger.Warn("READ: failed to get content store", "share", tree.ShareName, "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
	}

	// Get cache for share (optional - nil means no caching)
	fileCache := h.Registry.GetCacheForShare(tree.ShareName)

	// ========================================================================
	// Step 5: Build AuthContext and validate permissions
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("READ: failed to build auth context", "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 6: Check for symlink - generate MFsymlink content on-the-fly
	// ========================================================================

	file, err := metadataStore.GetFile(authCtx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("READ: failed to get file metadata", "path", openFile.Path, "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// Handle symlink reads - SMB clients expect MFsymlink content for symlinks
	if file.Type == metadata.FileTypeSymlink {
		return h.handleSymlinkRead(ctx, openFile, file, req)
	}

	// Validate read permission using PrepareRead (for regular files only)
	readMeta, err := metadataStore.PrepareRead(authCtx, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("READ: permission check failed", "path", openFile.Path, "error", err)
		return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// ========================================================================
	// Step 6.5: Check for conflicting byte-range locks
	// ========================================================================

	// Reads are blocked by another session's exclusive locks
	if err := metadataStore.CheckLockForIO(
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
	// Step 7: Handle empty file or offset beyond EOF
	// ========================================================================

	fileSize := readMeta.Attr.Size

	if readMeta.Attr.ContentID == "" || fileSize == 0 {
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
	// Step 8: Calculate read range
	// ========================================================================

	readEnd := req.Offset + uint64(req.Length)
	if readEnd > fileSize {
		readEnd = fileSize
	}
	actualLength := uint32(readEnd - req.Offset)

	// ========================================================================
	// Step 9: Read data (try cache first, then content store)
	// ========================================================================

	var data []byte
	var cacheHit bool

	// Try reading from cache first (if available)
	if fileCache != nil {
		cacheResult := tryReadFromCache(ctx.Context, fileCache, readMeta.Attr.ContentID, req.Offset, actualLength)
		if cacheResult.hit {
			data = cacheResult.data
			cacheHit = true
			logger.Debug("READ: cache hit",
				"path", openFile.Path,
				"state", cacheResult.state,
				"bytes", len(data))
		}
	}

	// Cache miss - read from content store
	if !cacheHit {
		// Try ReadAt if available (more efficient for partial reads)
		if readAtStore, ok := contentStore.(content.ReadAtContentStore); ok {
			data = make([]byte, actualLength)
			n, err := readAtStore.ReadAt(authCtx.Context, readMeta.Attr.ContentID, data, req.Offset)
			if err != nil {
				logger.Warn("READ: content read failed", "path", openFile.Path, "error", err)
				return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: ContentErrorToSMBStatus(err)}}, nil
			}
			data = data[:n]
		} else {
			// Fallback to ReadContent (reads entire file)
			reader, err := contentStore.ReadContent(authCtx.Context, readMeta.Attr.ContentID)
			if err != nil {
				logger.Warn("READ: content read failed", "path", openFile.Path, "error", err)
				return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: ContentErrorToSMBStatus(err)}}, nil
			}
			defer func() { _ = reader.Close() }()

			// Skip to offset by reading and discarding bytes
			if req.Offset > 0 {
				skipBuf := make([]byte, min(req.Offset, seekBufferSize))
				remaining := req.Offset
				for remaining > 0 {
					toRead := min(remaining, uint64(len(skipBuf)))
					n, err := reader.Read(skipBuf[:toRead])
					if err != nil {
						logger.Warn("READ: seek failed", "path", openFile.Path, "error", err)
						return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
					}
					remaining -= uint64(n)
				}
			}

			// Read requested data
			data = make([]byte, actualLength)
			totalRead := 0
			for totalRead < int(actualLength) {
				n, err := reader.Read(data[totalRead:])
				if err != nil && n == 0 {
					break
				}
				totalRead += n
			}
			data = data[:totalRead]
		}
	}

	// Log read result
	source := "content_store"
	if cacheHit {
		source = "cache"
	}
	logger.Debug("READ successful",
		"path", openFile.Path,
		"offset", req.Offset,
		"requested", req.Length,
		"actual", len(data),
		"source", source)

	// ========================================================================
	// Step 10: Return success response
	// ========================================================================

	return &ReadResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		DataOffset:      0x50, // Standard offset (header + response struct)
		Data:            data,
		DataRemaining:   0,
	}, nil
}

// ============================================================================
// Read Helper Functions
// ============================================================================

// cacheReadResult holds the result of attempting to read from cache.
type cacheReadResult struct {
	data  []byte
	state string
	hit   bool
}

// tryReadFromCache attempts to read data from the unified cache.
//
// Cache state handling:
//   - StateBuffering/StateUploading: Must read from cache (dirty data)
//   - StateCached: Read from cache (clean data)
//   - StatePrefetching/StateNone: Cache miss
//
// Parameters:
//   - ctx: Context for cancellation
//   - c: Cache instance
//   - contentID: Content identifier to read
//   - offset: Byte offset to read from
//   - length: Number of bytes to read
//
// Returns cacheReadResult with hit=true if data found, hit=false otherwise.
func tryReadFromCache(
	ctx context.Context,
	c cache.Cache,
	contentID metadata.ContentID,
	offset uint64,
	length uint32,
) cacheReadResult {
	state := c.GetState(contentID)

	switch state {
	case cache.StateBuffering, cache.StateUploading:
		// Dirty data in cache - must read from cache (content store may not have it yet)
		cacheSize := c.Size(contentID)
		if cacheSize > 0 {
			data := make([]byte, length)
			n, readErr := c.ReadAt(ctx, contentID, data, offset)

			if readErr == nil || readErr == io.EOF {
				logger.Debug("READ: cache hit (dirty)",
					"state", state.String(),
					"bytes_read", bytesize.ByteSize(n),
					"content_id", contentID)

				return cacheReadResult{
					data:  data[:n],
					state: state.String(),
					hit:   true,
				}
			}
		}

	case cache.StateCached:
		// Clean cached data - read from cache
		cacheSize := c.Size(contentID)
		if cacheSize > 0 {
			data := make([]byte, length)
			n, readErr := c.ReadAt(ctx, contentID, data, offset)

			if readErr == nil || readErr == io.EOF {
				logger.Debug("READ: cache hit (cached)",
					"bytes_read", bytesize.ByteSize(n),
					"content_id", contentID)

				return cacheReadResult{
					data:  data[:n],
					state: state.String(),
					hit:   true,
				}
			}
		}

	case cache.StatePrefetching, cache.StateNone:
		// Cache miss - data not available in cache
		logger.Debug("READ: cache miss",
			"state", state.String(),
			"content_id", contentID)
	}

	return cacheReadResult{hit: false}
}

// handleSymlinkRead generates MFsymlink content for a symlink read request.
//
// SMB clients (macOS, Windows) expect symlinks to be stored as MFsymlink files -
// regular files with a special XSym format containing the symlink target.
// This function generates that content on-the-fly from the symlink's LinkTarget.
//
// Parameters:
//   - ctx: SMB handler context
//   - openFile: The open file representing the symlink
//   - file: The file metadata (must have Type == FileTypeSymlink)
//   - req: The READ request containing offset and length
//
// Returns a READ response with the appropriate portion of MFsymlink content.
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
