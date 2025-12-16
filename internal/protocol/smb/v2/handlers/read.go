package handlers

import (
	"context"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Read handles SMB2 READ command [MS-SMB2] 2.2.19, 2.2.20
//
// **Purpose:**
//
// READ allows clients to read data from an open file at a specified offset.
// This is one of the most frequently called SMB2 operations.
//
// **Process:**
//
//  1. Decode request to extract FileID, offset, and length
//  2. Validate FileID maps to an open file (not a directory)
//  3. Get session and tree connection for context
//  4. Get metadata and content stores for the share
//  5. Build AuthContext and validate read permission via PrepareRead
//  6. Handle empty file or offset beyond EOF
//  7. Calculate actual read range (may be truncated at EOF)
//  8. Read data from content store (using ReadAt if available)
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
//   - StatusInvalidParameter: Malformed request
//   - StatusInvalidHandle: Invalid FileID
//   - StatusInvalidDeviceRequest: Cannot read from directory
//   - StatusUserSessionDeleted: Session no longer valid
//   - StatusAccessDenied: Read permission denied
//   - StatusBadNetworkName: Share not found
//   - StatusEndOfFile: Offset beyond file size
//   - StatusInternalError: Encoding error
//
// **Performance Considerations:**
//
// READ is frequently called and performance-critical:
//   - Uses ReadAt interface for efficient partial reads
//   - Avoids reading entire file when only a portion is needed
//   - SMB clients typically request 32KB-64KB chunks
//   - Parallel reads from different clients are supported
func (h *Handler) Read(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeReadRequest(body)
	if err != nil {
		logger.Debug("READ: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("READ request",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"offset", req.Offset,
		"length", req.Length)

	// ========================================================================
	// Step 2: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("READ: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// ========================================================================
	// Step 3: Validate file type
	// ========================================================================

	if openFile.IsDirectory {
		logger.Debug("READ: cannot read from directory", "path", openFile.Path)
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// ========================================================================
	// Step 4: Get session and tree connection
	// ========================================================================

	tree, ok := h.GetTree(openFile.TreeID)
	if !ok {
		logger.Debug("READ: invalid tree ID", "treeID", openFile.TreeID)
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	sess, ok := h.GetSession(openFile.SessionID)
	if !ok {
		logger.Debug("READ: invalid session ID", "sessionID", openFile.SessionID)
		return NewErrorResult(types.StatusUserSessionDeleted), nil
	}

	// Update context
	ctx.ShareName = tree.ShareName
	ctx.User = sess.User
	ctx.IsGuest = sess.IsGuest
	ctx.Permission = tree.Permission

	// ========================================================================
	// Step 5: Get metadata and content stores
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(tree.ShareName)
	if err != nil {
		logger.Warn("READ: failed to get metadata store", "share", tree.ShareName, "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	contentStore, err := h.Registry.GetContentStoreForShare(tree.ShareName)
	if err != nil {
		logger.Warn("READ: failed to get content store", "share", tree.ShareName, "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	// Get cache for share (optional - nil means no caching)
	fileCache := h.Registry.GetCacheForShare(tree.ShareName)

	// ========================================================================
	// Step 6: Build AuthContext and validate permissions
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("READ: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// Validate read permission using PrepareRead
	readMeta, err := metadataStore.PrepareRead(authCtx, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("READ: permission check failed", "path", openFile.Path, "error", err)
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	// ========================================================================
	// Step 7: Handle empty file or offset beyond EOF
	// ========================================================================

	fileSize := readMeta.Attr.Size

	if readMeta.Attr.ContentID == "" || fileSize == 0 {
		logger.Debug("READ: empty file", "path", openFile.Path)
		resp := &ReadResponse{
			DataOffset:    0x50, // Standard offset
			Data:          []byte{},
			DataRemaining: 0,
		}
		respBytes, _ := EncodeReadResponse(resp)
		return NewResult(types.StatusSuccess, respBytes), nil
	}

	if req.Offset >= fileSize {
		logger.Debug("READ: offset beyond EOF", "path", openFile.Path, "offset", req.Offset, "size", fileSize)
		return NewErrorResult(types.StatusEndOfFile), nil
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
				return NewErrorResult(ContentErrorToSMBStatus(err)), nil
			}
			data = data[:n]
		} else {
			// Fallback to ReadContent (reads entire file)
			reader, err := contentStore.ReadContent(authCtx.Context, readMeta.Attr.ContentID)
			if err != nil {
				logger.Warn("READ: content read failed", "path", openFile.Path, "error", err)
				return NewErrorResult(ContentErrorToSMBStatus(err)), nil
			}
			defer reader.Close()

			// Skip to offset
			if req.Offset > 0 {
				skipBuf := make([]byte, min(req.Offset, 8192))
				remaining := req.Offset
				for remaining > 0 {
					toRead := min(remaining, uint64(len(skipBuf)))
					n, err := reader.Read(skipBuf[:toRead])
					if err != nil {
						logger.Warn("READ: seek failed", "path", openFile.Path, "error", err)
						return NewErrorResult(types.StatusInternalError), nil
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
	// Step 10: Build and encode response
	// ========================================================================

	resp := &ReadResponse{
		DataOffset:    0x50, // Standard offset (header + response struct)
		Data:          data,
		DataRemaining: 0,
	}

	respBytes, err := EncodeReadResponse(resp)
	if err != nil {
		logger.Warn("READ: failed to encode response", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	return NewResult(types.StatusSuccess, respBytes), nil
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
