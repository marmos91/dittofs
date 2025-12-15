package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// ReadRequest represents a READ request from an NFS client.
// The client specifies a file handle, offset, and number of bytes to read.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.6 specifies the READ procedure as:
//
//	READ3res NFSPROC3_READ(READ3args) = 6;
//
// The READ procedure is used to read data from a file. It's one of the most
// fundamental and frequently called NFS operations.
type ReadRequest struct {
	// Handle is the file handle of the file to read from.
	// Must be a valid file handle for a regular file (not a directory).
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// Offset is the byte offset in the file to start reading from.
	// Can be any value from 0 to file size - 1.
	// Reading beyond EOF returns 0 bytes with Eof=true.
	Offset uint64

	// Count is the number of bytes to read.
	// The server may return fewer bytes than requested if:
	//   - EOF is encountered
	//   - Count exceeds server's maximum read size (rtmax from FSINFO)
	//   - Internal constraints apply
	Count uint32
}

// ReadResponse represents the response to a READ request.
// It contains the status, optional file attributes, and the data read.
//
// The response is encoded in XDR format before being sent back to the client.
type ReadResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains post-operation attributes of the file.
	// Optional, may be nil if Status != types.NFS3OK or attributes unavailable.
	// Helps clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Count is the actual number of bytes read.
	// May be less than requested if:
	//   - EOF was reached
	//   - Server constraints apply
	// Only present when Status == types.NFS3OK.
	Count uint32

	// Eof indicates whether the end of file was reached.
	// true: The read reached or passed the end of file
	// false: More data exists beyond the bytes returned
	// Only present when Status == types.NFS3OK.
	Eof bool

	// Data contains the actual bytes read from the file.
	// Length matches Count field.
	// Empty if Count == 0 or Status != types.NFS3OK.
	Data []byte
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Read reads data from a regular file.
//
// This implements the NFS READ procedure as defined in RFC 1813 Section 3.3.6.
//
// **Purpose:**
//
// READ is the fundamental operation for retrieving file data over NFS. It's used by:
//   - Applications reading file contents
//   - Editors loading files
//   - Compilers accessing source code
//   - Any operation that needs file data
//
// **Process:**
//
//  1. Check for context cancellation (client disconnect, timeout)
//  2. Validate request parameters (handle, offset, count)
//  3. Extract client IP and authentication credentials from context
//  4. Verify file exists and is a regular file (via store)
//  5. Check read permissions (delegated to store/content layer)
//  6. Open content for reading
//  7. Seek to requested offset (with cancellation checks)
//  8. Read requested number of bytes (with cancellation checks during read)
//  9. Detect EOF condition
//  10. Return data with updated file attributes
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - Content store handles actual data reading
//   - Metadata store provides file attributes and validation
//   - Access control enforced by store layers
//   - Context cancellation checked at key operation points
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// Read permission checking should be implemented by:
//   - store layer for file existence validation
//   - Content store layer for read access control
//
// **EOF Detection:**
//
// The server sets Eof=true when:
//   - The read operation reaches the end of the file
//   - The last byte of the file is included in the returned data
//   - offset + count >= file_size
//
// Clients use this to detect when they've read the entire file.
//
// **Context Cancellation:**
//
// READ operations can be time-consuming, especially for large files or slow storage.
// Context cancellation is checked at multiple points:
//   - Before starting the operation (client disconnect detection)
//   - After metadata lookup (before opening content)
//   - During seek operations (for non-seekable readers)
//   - During data reading (chunked reads for large transfers)
//
// Cancellation scenarios include:
//   - Client disconnects mid-transfer
//   - Client timeout expires
//   - Server shutdown initiated
//   - Network connection lost
//
// For large reads (>1MB), we use chunked reading with periodic cancellation checks
// to ensure responsive cancellation without excessive overhead.
//
// **Performance Considerations:**
//
// READ is one of the most frequently called NFS procedures. Implementations should:
//   - Use efficient content store access
//   - Support seekable readers when possible
//   - Minimize data copying
//   - Return reasonable chunk sizes (check FSINFO rtpref)
//   - Cache file attributes when possible
//   - Balance cancellation checks with performance (avoid checking too frequently)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store/Content errors are mapped to NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Not a regular file → types.NFS3ErrIsDir
//   - Permission denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Stale handle → NFS3ErrStale
//   - Context cancelled → returns context error (client disconnect)
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store/content layers enforce read permissions
//   - Client context enables audit logging
//   - No data leakage on permission errors
//   - Cancellation prevents resource exhaustion
//
// **Parameters:**
//   - ctx: Context with client address, authentication, and cancellation support
//   - contentStore: Content repository for file data access
//   - metadataStore: Metadata store for file attributes
//   - req: The read request containing handle, offset, and count
//
// **Returns:**
//   - *ReadResponse: Response with status, data, and attributes
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.6: READ Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &ReadRequest{
//	    Handle: fileHandle,
//	    Offset: 0,
//	    Count:  4096,
//	}
//	ctx := &NFSHandlerContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    Share:      "/export",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Read(ctx, contentStore, metadataStore, req)
//	if err == context.Canceled {
//	    // Client disconnected during read
//	    return nil, err
//	}
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Process resp.Data (resp.Count bytes)
//	    if resp.Eof {
//	        // End of file reached
//	    }
//	}
func (h *Handler) Read(
	ctx *NFSHandlerContext,
	req *ReadRequest,
) (*ReadResponse, error) {
	// ========================================================================
	// Context Cancellation Check - Entry Point
	// ========================================================================
	// Check if the client has disconnected or the request has timed out
	// before we start any expensive operations.
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "READ: request cancelled at entry", "handle", fmt.Sprintf("0x%x", req.Handle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "READ", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", bytesize.ByteSize(req.Offset), "count", bytesize.ByteSize(req.Count), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateReadRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "READ validation failed", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP, "error", err)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata and content stores from context
	// ========================================================================

	metadataStore, err := h.getMetadataStore(ctx)
	if err != nil {
		logger.WarnCtx(ctx.Context, "READ failed", "error", err, "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	contentStore, err := h.getContentStore(ctx)
	if err != nil {
		logger.WarnCtx(ctx.Context, "READ failed", "error", err, "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	logger.DebugCtx(ctx.Context, "READ: share", "share", ctx.Share)

	// ========================================================================
	// Step 3: Verify file exists and is a regular file
	// ========================================================================

	file, status, err := h.getFileOrError(ctx, metadataStore, fileHandle, "READ", req.Handle)
	if file == nil {
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Verify it's a regular file (not a directory or special file)
	if file.Type != metadata.FileTypeRegular {
		logger.WarnCtx(ctx.Context, "READ failed: not a regular file", "handle", fmt.Sprintf("0x%x", req.Handle), "type", file.Type, "client", clientIP)

		// Return file attributes even on error for cache consistency
		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIsDir}, // types.NFS3ErrIsDir is used for all non-regular files
			Attr:            nfsAttr,
		}, nil
	}

	// ========================================================================
	// Context Cancellation Check - After Metadata Lookup
	// ========================================================================
	// Check again before opening content (which may be expensive)
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "READ: request cancelled after metadata lookup", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 3: Check for empty file or invalid offset
	// ========================================================================

	// If file has no content, return empty data with EOF
	if file.ContentID == "" || file.Size == 0 {
		logger.DebugCtx(ctx.Context, "READ: empty file", "handle", fmt.Sprintf("0x%x", req.Handle), "size", bytesize.ByteSize(file.Size), "client", clientIP)

		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			Attr:            nfsAttr,
			Count:           0,
			Eof:             true,
			Data:            []byte{},
		}, nil
	}

	// If offset is at or beyond EOF, return empty data with EOF
	if req.Offset >= file.Size {
		logger.DebugCtx(ctx.Context, "READ: offset beyond EOF", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", bytesize.ByteSize(req.Offset), "size", bytesize.ByteSize(file.Size), "client", clientIP)

		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			Attr:            nfsAttr,
			Count:           0,
			Eof:             true,
			Data:            []byte{},
		}, nil
	}

	// ========================================================================
	// Step 4: Read content data (with read-through cache)
	// ========================================================================
	// Four read paths (in priority order):
	//   1. Write cache (if available and has data) - fastest, handles files being written
	//   2. Read cache (if available and has data) - fast, handles recently read files
	//   3. ReadAt (if content store supports it) - efficient for range reads
	//   4. ReadContent (fallback) - sequential read

	var data []byte
	var n int
	var eof bool
	var cacheHit bool

	// Try reading from cache first
	cacheResult, err := h.tryReadFromCache(ctx, file.ContentID, req.Offset, req.Count)
	if err != nil {
		traceError(ctx.Context, err, "READ failed", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	if cacheResult.hit {
		// Cache hit - use cached data
		data = cacheResult.data
		n = cacheResult.bytesRead
		eof = cacheResult.eof
		cacheHit = true
	} else {
		// Cache miss - read from content store
		var readResult contentStoreReadResult
		var readErr error

		// Check if content store supports efficient random-access reads
		if readAtStore, ok := contentStore.(content.ReadAtContentStore); ok {
			// FAST PATH: Use ReadAt for efficient range reads (S3, etc.)
			readResult, readErr = readFromContentStoreWithReadAt(ctx, readAtStore, file.ContentID, req.Offset, req.Count, clientIP, req.Handle)
		} else {
			// FALLBACK PATH: Use sequential ReadContent + Seek + Read
			readResult, readErr = readFromContentStoreSequential(ctx, contentStore, file.ContentID, req.Offset, req.Count, clientIP, req.Handle)
		}

		// Handle content store errors
		if readErr != nil {
			// Check if cancellation error
			if readErr == context.Canceled || readErr == context.DeadlineExceeded {
				return nil, readErr
			}

			// I/O error
			traceError(ctx.Context, readErr, "READ failed", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "client", clientIP)
			nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)
			return &ReadResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				Attr:            nfsAttr,
			}, nil
		}

		data = readResult.data
		n = readResult.bytesRead
		eof = readResult.eof

		// Start background prefetch to cache the entire file for future reads
		h.startBackgroundPrefetch(ctx, contentStore, file.ContentID, file.Size)
	}

	// Even if read succeeded, check if we're at or past EOF
	if req.Offset+uint64(n) >= file.Size {
		eof = true
	}

	// ========================================================================
	// Step 7: Build success response
	// ========================================================================

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

	// Log cache hit/miss for performance monitoring
	cacheSource := "content_store"
	if cacheHit {
		cacheSource = "cache"
	}

	logger.InfoCtx(ctx.Context, "READ successful", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", bytesize.ByteSize(req.Offset), "requested", bytesize.ByteSize(req.Count), "read", bytesize.ByteSize(n), "eof", eof, "source", cacheSource, "client", clientIP)

	logger.DebugCtx(ctx.Context, "READ details", "size", bytesize.ByteSize(file.Size), "type", nfsAttr.Type, "mode", fmt.Sprintf("%o", file.Mode))

	return &ReadResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
		Count:           uint32(n),
		Eof:             eof,
		Data:            data,
	}, nil
}

// ============================================================================
// Read Helper Functions
// ============================================================================

// cacheReadResult holds the result of attempting to read from cache.
type cacheReadResult struct {
	data      []byte
	bytesRead int
	eof       bool
	hit       bool // true if data was found in cache
}

// tryReadFromCache attempts to read data from the unified cache.
// Returns cache hit result if successful, or empty result if cache miss.
//
// Cache state handling:
//   - StateBuffering/StateUploading: Read from cache (dirty data, highest priority)
//   - StateCached: Read from cache (clean data)
//   - StatePrefetching: Cache miss (prefetch in progress, read from content store)
//   - StateNone: Cache miss
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - contentID: Content identifier to read
//   - offset: Byte offset to read from
//   - count: Number of bytes to read
//
// Returns:
//   - cacheReadResult: Result with data if cache hit, empty if cache miss
//   - error: Error if cache read failed (cache miss returns nil error)
func (h *Handler) tryReadFromCache(
	ctx *NFSHandlerContext,
	contentID metadata.ContentID,
	offset uint64,
	count uint32,
) (cacheReadResult, error) {
	c := h.Registry.GetCacheForShare(ctx.Share)
	if c == nil {
		// No cache configured
		return cacheReadResult{hit: false}, nil
	}

	state := c.GetState(contentID)

	switch state {
	case cache.StateBuffering, cache.StateUploading:
		// Dirty data in cache - must read from cache (content store may not have it yet)
		cacheSize := c.Size(contentID)
		if cacheSize > 0 {
			logger.DebugCtx(ctx.Context, "READ: reading dirty data from cache", "state", state, "offset", bytesize.ByteSize(offset), "count", bytesize.ByteSize(count), "cache_size", bytesize.ByteSize(cacheSize), "content_id", contentID)

			data := make([]byte, count)
			n, readErr := c.ReadAt(ctx.Context, contentID, data, offset)

			if readErr == nil || readErr == io.EOF {
				eof := (readErr == io.EOF) || (offset+uint64(n) >= cacheSize)
				logger.DebugCtx(ctx.Context, "READ: cache hit (dirty)", "bytes_read", bytesize.ByteSize(n), "eof", eof, "content_id", contentID)

				if h.Metrics != nil {
					h.Metrics.RecordCacheHit(ctx.Share, "dirty", uint64(n))
				}

				return cacheReadResult{
					data:      data[:n],
					bytesRead: n,
					eof:       eof,
					hit:       true,
				}, nil
			}

			logger.WarnCtx(ctx.Context, "READ: cache read error (dirty data), this is unexpected", "content_id", contentID, "error", readErr)
			// Fall through to content store - but this shouldn't happen for dirty data
		}

	case cache.StateCached:
		// Clean data in cache - read from cache
		cacheSize := c.Size(contentID)
		if cacheSize > 0 {
			logger.DebugCtx(ctx.Context, "READ: reading from cache", "offset", bytesize.ByteSize(offset), "count", bytesize.ByteSize(count), "cache_size", bytesize.ByteSize(cacheSize), "content_id", contentID)

			data := make([]byte, count)
			n, readErr := c.ReadAt(ctx.Context, contentID, data, offset)

			if readErr == nil || readErr == io.EOF {
				eof := (readErr == io.EOF) || (offset+uint64(n) >= cacheSize)
				logger.DebugCtx(ctx.Context, "READ: cache hit", "bytes_read", bytesize.ByteSize(n), "eof", eof, "content_id", contentID)

				if h.Metrics != nil {
					h.Metrics.RecordCacheHit(ctx.Share, "clean", uint64(n))
				}

				return cacheReadResult{
					data:      data[:n],
					bytesRead: n,
					eof:       eof,
					hit:       true,
				}, nil
			}

			logger.WarnCtx(ctx.Context, "READ: cache read error, falling back to content store", "content_id", contentID, "error", readErr)
		}

	case cache.StatePrefetching:
		// Prefetch in progress - wait for the required offset to be available
		requiredOffset := offset + uint64(count)
		logger.DebugCtx(ctx.Context, "READ: prefetch in progress, waiting for offset", "required_offset", bytesize.ByteSize(requiredOffset), "content_id", contentID)

		if err := c.WaitForPrefetchOffset(ctx.Context, contentID, requiredOffset); err != nil {
			return cacheReadResult{hit: false}, err
		}

		// Our bytes are now available - read from cache
		cacheSize := c.Size(contentID)
		data := make([]byte, count)
		n, readErr := c.ReadAt(ctx.Context, contentID, data, offset)

		if readErr == nil || readErr == io.EOF {
			eof := (readErr == io.EOF) || (offset+uint64(n) >= cacheSize)
			logger.DebugCtx(ctx.Context, "READ: cache hit after prefetch", "bytes_read", bytesize.ByteSize(n), "eof", eof, "content_id", contentID)

			if h.Metrics != nil {
				h.Metrics.RecordCacheHit(ctx.Share, "prefetch", uint64(n))
			}

			return cacheReadResult{
				data:      data[:n],
				bytesRead: n,
				eof:       eof,
				hit:       true,
			}, nil
		}

		logger.WarnCtx(ctx.Context, "READ: cache read error after prefetch wait", "content_id", contentID, "error", readErr)
		// Fall through to cache miss

	case cache.StateNone:
		// Not in cache
		logger.DebugCtx(ctx.Context, "READ: cache miss", "content_id", contentID)
	}

	// Cache miss
	if h.Metrics != nil {
		h.Metrics.RecordCacheMiss(ctx.Share, uint64(count))
	}

	return cacheReadResult{hit: false}, nil
}

// Prefetch configuration defaults (used when config values are zero)
const (
	// defaultMaxPrefetchSize is the maximum file size to prefetch.
	// Files larger than this are not prefetched to avoid cache thrashing.
	defaultMaxPrefetchSize = 100 * 1024 * 1024 // 100MB

	// defaultPrefetchChunkSize is the size of each chunk read during prefetch.
	// Larger chunks = fewer requests but longer wait before unblocking reads.
	// Smaller chunks = more requests but faster unblocking of waiting reads.
	defaultPrefetchChunkSize = 512 * 1024 // 512KB
)

// startBackgroundPrefetch starts a background goroutine to fetch the entire file into cache.
//
// This is called on cache miss to prefetch the file for future reads.
// The prefetch runs asynchronously - the current READ request has already been served
// from the content store directly.
//
// Prefetch is skipped if:
//   - No cache is configured for this share
//   - Prefetch is disabled for this share
//   - File is too large (> maxPrefetchSize from config)
//   - Prefetch is already in progress for this content ID
func (h *Handler) startBackgroundPrefetch(
	ctx *NFSHandlerContext,
	contentStore content.ContentStore,
	contentID metadata.ContentID,
	fileSize uint64,
) {
	c := h.Registry.GetCacheForShare(ctx.Share)
	if c == nil {
		return // No cache configured
	}

	// Get share to access prefetch config
	share, err := h.Registry.GetShare(ctx.Share)
	if err != nil {
		return // Share not found
	}

	// Check if prefetch is enabled
	if !share.PrefetchConfig.Enabled {
		return // Prefetch disabled
	}

	// Get max file size from config (use default if not set)
	maxFileSize := share.PrefetchConfig.MaxFileSize
	if maxFileSize == 0 {
		maxFileSize = defaultMaxPrefetchSize
	}

	// Skip large files to avoid cache thrashing
	if fileSize > uint64(maxFileSize) {
		logger.DebugCtx(ctx.Context, "READ: skipping prefetch for large file", "content_id", contentID, "size", bytesize.ByteSize(fileSize), "max", bytesize.ByteSize(maxFileSize))
		return
	}

	// Try to start prefetch - returns false if already in progress or not needed
	if !c.StartPrefetch(contentID, fileSize) {
		logger.DebugCtx(ctx.Context, "READ: prefetch already in progress or not needed", "content_id", contentID)
		return
	}

	// Get chunk size from config (use default if not set)
	chunkSize := share.PrefetchConfig.ChunkSize
	if chunkSize == 0 {
		chunkSize = defaultPrefetchChunkSize
	}

	logger.DebugCtx(ctx.Context, "READ: starting background prefetch", "content_id", contentID, "size", bytesize.ByteSize(fileSize), "chunk_size", bytesize.ByteSize(chunkSize))

	// Spawn background goroutine to fetch the file
	go h.runPrefetch(ctx.Share, contentStore, contentID, fileSize, chunkSize)
}

// runPrefetch fetches the entire file content and writes it to cache.
//
// This runs in a background goroutine. It reads the file in chunks,
// updating the prefetched offset after each chunk so that waiting
// READ requests can be served as soon as their bytes are available.
func (h *Handler) runPrefetch(
	share string,
	contentStore content.ContentStore,
	contentID metadata.ContentID,
	fileSize uint64,
	chunkSize int64,
) {
	c := h.Registry.GetCacheForShare(share)
	if c == nil {
		return // Cache was removed
	}

	// Use a background context - prefetch should continue even if original request is done
	ctx := context.Background()

	var offset uint64
	success := false

	defer func() {
		c.CompletePrefetch(contentID, success)
		if success {
			logger.Debug("READ: prefetch completed", "content_id", contentID, "size", bytesize.ByteSize(fileSize))
		} else {
			logger.Warn("READ: prefetch failed", "content_id", contentID)
		}
	}()

	// Check if content store supports ReadAt for efficient chunked reads
	readAtStore, hasReadAt := contentStore.(content.ReadAtContentStore)

	if hasReadAt {
		// Efficient path: read in chunks using ReadAt
		for offset < fileSize {
			remaining := fileSize - offset
			readSize := min(remaining, uint64(chunkSize))

			chunk := make([]byte, readSize)
			n, err := readAtStore.ReadAt(ctx, contentID, chunk, offset)
			if err != nil && err != io.EOF {
				logger.Warn("READ: prefetch chunk read failed", "content_id", contentID, "offset", offset, "error", err)
				return
			}

			if n > 0 {
				// Write chunk to cache
				if err := c.WriteAt(ctx, contentID, chunk[:n], offset); err != nil {
					logger.Warn("READ: prefetch cache write failed", "content_id", contentID, "offset", offset, "error", err)
					return
				}

				offset += uint64(n)
				c.SetPrefetchedOffset(contentID, offset)
			}

			if err == io.EOF || n == 0 {
				break
			}
		}
	} else {
		// Fallback: read entire content using streaming reader
		reader, err := contentStore.ReadContent(ctx, contentID)
		if err != nil {
			logger.Warn("READ: prefetch read failed", "content_id", contentID, "error", err)
			return
		}
		defer func() { _ = reader.Close() }()

		// Read and write in chunks
		for {
			chunk := make([]byte, chunkSize)
			n, err := reader.Read(chunk)

			if n > 0 {
				if writeErr := c.WriteAt(ctx, contentID, chunk[:n], offset); writeErr != nil {
					logger.Warn("READ: prefetch cache write failed", "content_id", contentID, "offset", offset, "error", writeErr)
					return
				}

				offset += uint64(n)
				c.SetPrefetchedOffset(contentID, offset)
			}

			if err == io.EOF {
				break
			}
			if err != nil {
				logger.Warn("READ: prefetch read failed", "content_id", contentID, "offset", offset, "error", err)
				return
			}
		}
	}

	success = true
}

// contentStoreReadResult holds the result of reading from content store.
type contentStoreReadResult struct {
	data      []byte
	bytesRead int
	eof       bool
}

// readFromContentStoreWithReadAt reads data using the ReadAt interface for efficient range reads.
// This is dramatically more efficient for backends like S3.
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - readAtStore: Content store that supports ReadAt
//   - contentID: Content identifier to read
//   - offset: Byte offset to read from
//   - count: Number of bytes to read
//   - clientIP: Client IP for logging
//   - handle: File handle for logging
//
// Returns:
//   - contentStoreReadResult: Result with data
//   - error: Error if read failed
func readFromContentStoreWithReadAt(
	ctx *NFSHandlerContext,
	readAtStore content.ReadAtContentStore,
	contentID metadata.ContentID,
	offset uint64,
	count uint32,
	clientIP string,
	handle []byte,
) (contentStoreReadResult, error) {
	logger.DebugCtx(ctx.Context, "READ: using content store ReadAt path", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "count", count, "content_id", contentID)

	data := make([]byte, count)
	n, readErr := readAtStore.ReadAt(ctx.Context, contentID, data, offset)

	// Handle ReadAt results
	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return contentStoreReadResult{
			data:      data[:n],
			bytesRead: n,
			eof:       true,
		}, nil
	}

	if readErr == context.Canceled || readErr == context.DeadlineExceeded {
		logger.DebugCtx(ctx.Context, "READ: request cancelled during ReadAt", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "read", n, "client", clientIP)
		return contentStoreReadResult{}, readErr
	}

	if readErr != nil {
		return contentStoreReadResult{}, fmt.Errorf("ReadAt error: %w", readErr)
	}

	return contentStoreReadResult{
		data:      data,
		bytesRead: n,
		eof:       false,
	}, nil
}

// seekToOffset seeks or discards bytes to reach the requested offset in a reader.
// Handles both seekable and non-seekable readers.
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - reader: Reader to seek (may or may not support io.Seeker)
//   - offset: Target offset
//   - clientIP: Client IP for logging
//   - handle: File handle for logging
//
// Returns:
//   - error: Error if seek/discard failed
func seekToOffset(
	ctx *NFSHandlerContext,
	reader io.ReadCloser,
	offset uint64,
	clientIP string,
	handle []byte,
) error {
	if offset == 0 {
		return nil // Already at start
	}

	if seeker, ok := reader.(io.Seeker); ok {
		// Reader supports seeking - use efficient seek
		_, err := seeker.Seek(int64(offset), io.SeekStart)
		if err != nil {
			return fmt.Errorf("seek error: %w", err)
		}
		return nil
	}

	// Reader doesn't support seeking - read and discard bytes
	logger.DebugCtx(ctx.Context, "READ: reader not seekable, discarding bytes", "bytes", bytesize.ByteSize(offset))

	// Use chunked discard with cancellation checks for large offsets
	const discardChunkSize = 64 * 1024 // 64KB chunks
	remaining := int64(offset)
	totalDiscarded := int64(0)

	for remaining > 0 {
		// Check for cancellation during discard
		select {
		case <-ctx.Context.Done():
			logger.DebugCtx(ctx.Context, "READ: request cancelled during seek discard", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "discarded", totalDiscarded, "client", clientIP)
			return ctx.Context.Err()
		default:
			// Continue
		}

		// Discard in chunks
		chunkSize := discardChunkSize
		if remaining < int64(chunkSize) {
			chunkSize = int(remaining)
		}

		discardN, discardErr := io.CopyN(io.Discard, reader, int64(chunkSize))
		totalDiscarded += discardN
		remaining -= discardN

		if discardErr == io.EOF {
			return io.EOF // EOF reached while seeking
		}

		if discardErr != nil {
			return fmt.Errorf("cannot skip to offset: %w", discardErr)
		}
	}

	return nil
}

// readFromContentStoreSequential reads data using sequential ReadContent + Seek + Read.
// This is a fallback for content stores that don't support ReadAt.
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - contentStore: Content store to read from
//   - contentID: Content identifier to read
//   - offset: Byte offset to read from
//   - count: Number of bytes to read
//   - clientIP: Client IP for logging
//   - handle: File handle for logging
//
// Returns:
//   - contentStoreReadResult: Result with data
//   - error: Error if read failed
func readFromContentStoreSequential(
	ctx *NFSHandlerContext,
	contentStore content.ContentStore,
	contentID metadata.ContentID,
	offset uint64,
	count uint32,
	clientIP string,
	handle []byte,
) (contentStoreReadResult, error) {
	logger.DebugCtx(ctx.Context, "READ: using sequential read path (no ReadAt support)", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "count", count)

	reader, err := contentStore.ReadContent(ctx.Context, contentID)
	if err != nil {
		return contentStoreReadResult{}, fmt.Errorf("cannot open content: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Seek to requested offset
	if err := seekToOffset(ctx, reader, offset, clientIP, handle); err != nil {
		if err == io.EOF {
			// EOF reached while seeking - return empty with EOF
			logger.DebugCtx(ctx.Context, "READ: EOF reached while seeking", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "client", clientIP)
			return contentStoreReadResult{
				data:      []byte{},
				bytesRead: 0,
				eof:       true,
			}, nil
		}
		return contentStoreReadResult{}, err
	}

	// Read requested data
	data := make([]byte, count)

	// For large reads (>1MB), use chunked reading with cancellation checks
	const largeReadThreshold = 1024 * 1024 // 1MB
	var n int
	var readErr error

	if count > largeReadThreshold {
		n, readErr = readWithCancellation(ctx.Context, reader, data)
	} else {
		n, readErr = io.ReadFull(reader, data)
	}

	// Handle read results
	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return contentStoreReadResult{
			data:      data[:n],
			bytesRead: n,
			eof:       true,
		}, nil
	}

	if readErr == context.Canceled || readErr == context.DeadlineExceeded {
		logger.DebugCtx(ctx.Context, "READ: request cancelled during data read", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "read", n, "client", clientIP)
		return contentStoreReadResult{}, readErr
	}

	if readErr != nil {
		return contentStoreReadResult{}, fmt.Errorf("I/O error: %w", readErr)
	}

	return contentStoreReadResult{
		data:      data,
		bytesRead: n,
		eof:       false,
	}, nil
}

// readWithCancellation reads data from a reader with periodic context cancellation checks.
// This is used for large reads to ensure responsive cancellation without checking
// on every byte.
//
// The function reads in chunks, checking for cancellation between chunks to balance
// performance with responsiveness.
//
// Parameters:
//   - ctx: Context for cancellation detection
//   - reader: Source to read from
//   - buf: Destination buffer to fill
//
// Returns:
//   - int: Number of bytes actually read
//   - error: Any error encountered (including context cancellation)
func readWithCancellation(ctx context.Context, reader io.Reader, buf []byte) (int, error) {
	const chunkSize = 256 * 1024 // 256KB chunks for cancellation checks

	totalRead := 0
	remaining := len(buf)

	for remaining > 0 {
		// Check for cancellation before each chunk
		select {
		case <-ctx.Done():
			// Return what we've read so far along with context error
			return totalRead, ctx.Err()
		default:
			// Continue reading
		}

		// Determine chunk size for this iteration
		readSize := min(remaining, chunkSize)

		// Read chunk
		n, err := io.ReadFull(reader, buf[totalRead:totalRead+readSize])
		totalRead += n
		remaining -= n

		if err != nil {
			// Return total read and the error (could be EOF, io.ErrUnexpectedEOF, or I/O error)
			return totalRead, err
		}
	}

	return totalRead, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// readValidationError represents a READ request validation error.
type readValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *readValidationError) Error() string {
	return e.message
}

// validateReadRequest validates READ request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - File handle is long enough for file ID extraction
//   - Count is not zero (RFC 1813 allows it, but it's unusual)
//   - Count doesn't exceed reasonable limits
//
// Returns:
//   - nil if valid
//   - *readValidationError with NFS status if invalid
func validateReadRequest(req *ReadRequest) *readValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &readValidationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &readValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &readValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate count - zero is technically valid but unusual
	if req.Count == 0 {
		logger.Debug("READ request with count=0 (unusual but valid)")
	}

	// Validate count doesn't exceed reasonable limits (1GB)
	// While RFC 1813 doesn't specify a maximum, extremely large reads should be rejected
	const maxReadSize = 1024 * 1024 * 1024 // 1GB
	if req.Count > maxReadSize {
		return &readValidationError{
			message:   fmt.Sprintf("read count too large: %d bytes (max %d)", req.Count, maxReadSize),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeReadRequest decodes a READ request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.6 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the READ request
//
// Returns:
//   - *ReadRequest: The decoded request containing handle, offset, and count
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READ request from network
//	req, err := DecodeReadRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Count in READ procedure
func DecodeReadRequest(data []byte) (*ReadRequest, error) {
	// Validate minimum data length
	// 4 bytes (handle length) + 8 bytes (offset) + 4 bytes (count) = 16 bytes minimum
	if len(data) < 16 {
		return nil, fmt.Errorf("data too short: need at least 16 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode file handle
	// ========================================================================

	// Read handle length (4 bytes, big-endian)
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("failed to read handle length: %w", err)
	}

	// Validate handle length
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// PERFORMANCE OPTIMIZATION: Use stack-allocated buffer for file handles
	// File handles are max 64 bytes per RFC 1813, so we can avoid heap allocation
	var handleBuf [64]byte
	handleSlice := handleBuf[:handleLen]
	if err := binary.Read(reader, binary.BigEndian, &handleSlice); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}
	// Make a copy to return (original stack buffer will be reused)
	handle := make([]byte, handleLen)
	copy(handle, handleSlice)

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read handle padding byte %d: %w", i, err)
		}
	}

	// ========================================================================
	// Decode offset
	// ========================================================================

	var offset uint64
	if err := binary.Read(reader, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("failed to read offset: %w", err)
	}

	// ========================================================================
	// Decode count
	// ========================================================================

	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("failed to read count: %w", err)
	}

	logger.Debug("Decoded READ request", "handle_len", handleLen, "offset", offset, "count", count)

	return &ReadRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.6 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op attributes (present flag + attributes if present)
//  3. If status == types.NFS3OK:
//     a. Count (4 bytes, big-endian uint32)
//     b. Eof flag (4 bytes, big-endian bool as uint32)
//     c. Data length (4 bytes, big-endian uint32)
//     d. Data bytes (variable length)
//     e. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding requires all data to be in big-endian format and aligned
// to 4-byte boundaries.
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
//
// Example:
//
//	resp := &ReadResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    Attr:   fileAttr,
//	    Count:  1024,
//	    Eof:    false,
//	    Data:   dataBytes,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadResponse) Encode() ([]byte, error) {
	// PERFORMANCE OPTIMIZATION: Pre-allocate buffer with estimated size
	// to avoid multiple allocations during encoding.
	//
	// Size calculation:
	//   - Status: 4 bytes
	//   - Optional file (present): 1 byte + ~84 bytes (NFSFileAttr)
	//   - Count: 4 bytes
	//   - EOF: 4 bytes
	//   - Data length: 4 bytes
	//   - Data: resp.Count bytes
	//   - Padding: 0-3 bytes
	//   Total: ~105 + data length + padding
	estimatedSize := 110 + int(resp.Count) + 3
	buf := bytes.NewBuffer(make([]byte, 0, estimatedSize))

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op attributes (both success and error cases)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return early if status is not OK
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READ error response", "status", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write count, EOF flag, and data
	// ========================================================================

	// Write count (number of bytes read)
	if err := binary.Write(buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("failed to write count: %w", err)
	}

	// Write EOF flag (boolean as uint32: 0=false, 1=true)
	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("failed to write eof flag: %w", err)
	}

	// Write data as opaque (length + data + padding)
	if err := xdr.WriteXDROpaque(buf, resp.Data); err != nil {
		return nil, fmt.Errorf("failed to write data: %w", err)
	}

	logger.Debug("Encoded READ response", "bytes_total", buf.Len(), "data_bytes", len(resp.Data), "status", resp.Status)

	return buf.Bytes(), nil
}
