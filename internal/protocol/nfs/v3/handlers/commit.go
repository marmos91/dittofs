package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// CommitRequest represents a COMMIT request from an NFS client.
// The client requests that the server flush any cached writes for a specified
// range of a file to stable storage.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.21 specifies the COMMIT procedure as:
//
//	COMMIT3res NFSPROC3_COMMIT(COMMIT3args) = 21;
//
// COMMIT ensures that data previously written with WRITE operations using
// the UNSTABLE storage option is committed to stable storage. This is
// critical for maintaining data integrity across server crashes.
type CommitRequest struct {
	// Handle is the file handle of the file to commit.
	// Must be a valid file handle obtained from CREATE, LOOKUP, etc.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// Offset is the starting byte offset of the region to commit.
	// If 0, commit starts from the beginning of the file.
	// Must be less than or equal to the file size.
	Offset uint64

	// Count is the number of bytes to commit starting from Offset.
	// If 0, commit from Offset to the end of the file.
	// The range [Offset, Offset+Count) should be committed.
	Count uint32
}

// CommitResponse represents the response to a COMMIT request.
// It contains the status of the operation, WCC data for the file,
// and a write verifier for detecting server state changes.
//
// The response is encoded in XDR format before being sent back to the client.
type CommitResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// AttrBefore contains pre-operation attributes of the file.
	// Used for weak cache consistency to detect concurrent changes.
	// May be nil if attributes could not be captured.
	AttrBefore *types.WccAttr

	// AttrAfter contains post-operation attributes of the file.
	// Used for weak cache consistency and to provide updated file state.
	// May be nil on error, but should be present on success.
	AttrAfter *types.NFSFileAttr

	// WriteVerifier is a unique value that changes when the server restarts.
	// Clients use this to detect server reboots and resubmit unstable writes.
	// Per RFC 1813: "The server is free to accept the data written by the client
	// and not write it to stable storage. The client can use the COMMIT operation
	// to force the server to write the data to stable storage."
	//
	// In this implementation:
	//   - Value of 0 indicates no server reboot tracking
	//   - A production implementation should track server start time or boot ID
	WriteVerifier uint64
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Commit forces cached writes to stable storage for a file or file range.
//
// This implements the NFS COMMIT procedure as defined in RFC 1813 Section 3.3.21.
//
// **Purpose:**
//
// COMMIT is a companion to WRITE with the UNSTABLE storage option. When clients
// write data with UNSTABLE storage, the server may cache the data in memory for
// performance. COMMIT forces the server to flush that cached data to stable
// storage (disk), ensuring durability.
//
// Key use cases:
//   - File synchronization after buffered writes
//   - Ensuring data durability before closing files
//   - Implementing fsync() or fdatasync() semantics
//   - Transaction commit points
//
// **Process:**
//
//  1. Check for context cancellation
//  2. Validate request parameters (handle format, offset/count range)
//  3. Extract client IP and authentication from context
//  4. Verify file exists and capture pre-operation state
//  5. Delegate commit operation to store (when implemented)
//  6. Return file WCC data and write verifier
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - Business logic (actual flushing) will be delegated to store
//   - File handle validation performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//   - Respects context cancellation for graceful shutdown and timeouts
//
// **Current Implementation:**
//
// This implementation is a no-op because the in-memory metadata store
// doesn't have write caching. All writes are immediately "committed" to the
// in-memory data structures.
//
// **Future Production Implementation:**
//
// A production implementation with actual write caching should:
//
//  1. Add a CommitFile method to the Store interface:
//     CommitFile(handle FileHandle, offset uint64, count uint32, ctx *AuthContext) error
//
//  2. The store should:
//     - Identify cached writes in the specified range [offset, offset+count)
//     - Flush those writes to stable storage (disk)
//     - Ensure durability (fsync, fdatasync, or equivalent)
//     - Update file metadata if needed (size, timestamps)
//     - Handle partial failures gracefully
//     - Respect context cancellation during long flush operations
//
//  3. Consider write cache coherency:
//     - Track which byte ranges have unstable writes
//     - Handle overlapping commit ranges
//     - Deal with commits during active writes
//
//  4. Implement write verifier tracking:
//     - Generate verifier on server start (e.g., boot timestamp)
//     - Return consistent verifier across all WRITE/COMMIT operations
//     - Change verifier on server restart to invalidate client caches
//
// **Atomicity and Consistency:**
//
// COMMIT should be atomic for the specified range:
//   - Either all data in the range is committed to stable storage
//   - Or the operation fails and returns an error status
//   - Partial commits should not be visible to clients
//
// **Special Cases:**
//
//   - Offset=0, Count=0: Commit entire file
//   - Count=0: Commit from Offset to end of file
//   - Range beyond file size: Commit what exists, ignore rest (not an error)
//   - Directory handle: Return types.NFS3ErrIsDir
//   - No unstable writes: Success (idempotent no-op)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Stale handle → types.NFS3ErrStale
//   - I/O error during flush → types.NFS3ErrIO
//   - Directory handle → types.NFS3ErrIsDir
//   - Invalid handle → types.NFS3ErrBadHandle
//   - Context cancelled → types.NFS3ErrIO (no specific NFS code for cancellation)
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps clients maintain cache coherency:
//  1. Capture file attributes before commit (WccBefore)
//  2. Perform the commit operation
//  3. Capture file attributes after commit (WccAfter)
//
// Clients use this to:
//   - Detect if file changed during commit
//   - Update cached file attributes
//   - Invalidate stale cached data
//
// **Write Verifier:**
//
// The write verifier is critical for handling server crashes:
//
//  1. Server returns same verifier for all WRITE/COMMIT operations
//  2. When server restarts, verifier changes
//  3. Client compares verifiers to detect server restart
//  4. If verifier changed, client must resend all unstable writes
//
// This ensures clients don't lose data when the server crashes before
// committing unstable writes to stable storage.
//
// **Performance Considerations:**
//
//   - COMMIT can be expensive (triggers disk I/O and fsync)
//   - Clients should batch commits when possible
//   - Server may optimize by tracking dirty ranges
//   - Consider group commits for multiple clients
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Range validation prevents integer overflow attacks
//   - Client context enables audit logging
//   - No special permission checks (commit doesn't modify data)
//
// **Context Cancellation:**
//
// This operation respects context cancellation:
//   - Checks at operation start before any work
//   - Checks before metadata store calls
//   - Returns types.NFS3ErrIO on cancellation (NFS has no specific cancel status)
//
// For a lightweight metadata operation like this, cancellation is primarily
// useful for:
//   - Handling client disconnects
//   - Supporting server shutdown
//   - Enforcing request timeouts
//
// **Parameters:**
//   - store: The metadata store for file operations
//   - req: The commit request containing file handle and range
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *CommitResponse: Response with status, WCC data, and write verifier
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.21: COMMIT Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &CommitRequest{
//	    Handle: fileHandle,
//	    Offset: 0,
//	    Count:  0, // Commit entire file
//	}
//	ctx := &CommitContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Commit(store, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Data committed successfully
//	}
func (h *Handler) Commit(
	ctx *NFSHandlerContext,
	req *CommitRequest,
) (*CommitResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("COMMIT: handle=%x offset=%d count=%d client=%s auth=%d",
		req.Handle, req.Offset, req.Count, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.Warn("COMMIT cancelled: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, ctx.Context.Err())
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateCommitRequest(req); err != nil {
		logger.Warn("COMMIT validation failed: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, err)
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 3: Get metadata store from context
	// ========================================================================

	store, err := h.getMetadataStore(ctx)
	if err != nil {
		logger.Warn("COMMIT failed: %v handle=%x client=%s", err, req.Handle, clientIP)
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	handle := metadata.FileHandle(req.Handle)

	// ========================================================================
	// Step 4: Verify file exists and capture pre-operation state
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.Warn("COMMIT cancelled before GetFile: handle=%x client=%s error=%v",
			req.Handle, clientIP, ctx.Context.Err())
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		logger.Warn("COMMIT failed: file not found: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(&file.FileAttr)

	// Verify this is not a directory
	if file.Type == metadata.FileTypeDirectory {
		logger.Warn("COMMIT failed: handle is a directory: handle=%x client=%s",
			req.Handle, clientIP)

		wccAfter := h.convertFileAttrToNFS(handle, &file.FileAttr)

		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIsDir},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4: Perform commit operation - flush write cache to content store
	// ========================================================================

	// Check context before potentially long flush operation
	if ctx.isContextCancelled() {
		logger.Warn("COMMIT cancelled before flush: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, ctx.Context.Err())

		// Get updated attributes for WCC data (best effort)
		var wccAfter *types.NFSFileAttr
		if updatedFile, getErr := store.GetFile(ctx.Context, handle); getErr == nil {
			wccAfter = h.convertFileAttrToNFS(handle, &updatedFile.FileAttr)
		}

		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, nil
	}

	// Get unified cache for this share (may be nil if sync mode)
	cache := h.Registry.GetCacheForShare(ctx.Share)

	// Convert file attributes for WCC "after" data
	wccAfter := h.convertFileAttrToNFS(handle, &file.FileAttr)

	if cache == nil {
		// Sync mode: data was written directly to content store, nothing to flush
		logger.Debug("COMMIT: sync mode (no cache), returning success")
		logger.Info("COMMIT successful: handle=%x offset=%d count=%d client=%s (sync mode)",
			req.Handle, req.Offset, req.Count, clientIP)
		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
			WriteVerifier:   serverBootTime,
		}, nil
	}

	// Async mode: check if there's data to flush
	if cache.Size(file.ContentID) == 0 {
		logger.Debug("COMMIT: no data in cache, returning success")
		logger.Info("COMMIT successful: handle=%x offset=%d count=%d client=%s (empty cache)",
			req.Handle, req.Offset, req.Count, clientIP)
		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
			WriteVerifier:   serverBootTime,
		}, nil
	}

	// Flush cache to content store
	logger.Info("COMMIT: flushing cache to content store for share=%s", ctx.Share)

	contentStore, err := h.Registry.GetContentStoreForShare(ctx.Share)
	if err != nil {
		logger.Error("COMMIT failed: cannot get content store: share=%s handle=%x client=%s error=%v",
			ctx.Share, req.Handle, clientIP, err)
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Flush cache to content store
	flushErr := flushCacheToContentStore(ctx, h, cache, contentStore, file, req)
	if flushErr != nil {
		logger.Error("COMMIT failed: flush error: handle=%x content_id=%s client=%s error=%v",
			req.Handle, file.ContentID, clientIP, flushErr)

		// Try to get updated attributes for error response
		if updatedFile, getErr := store.GetFile(ctx.Context, handle); getErr == nil {
			wccAfter = h.convertFileAttrToNFS(handle, &updatedFile.FileAttr)
		}

		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, nil
	}

	// Get updated file attributes for WCC data (file may have changed after flush)
	if updatedFile, getErr := store.GetFile(ctx.Context, handle); getErr == nil {
		wccAfter = h.convertFileAttrToNFS(handle, &updatedFile.FileAttr)
		logger.Debug("COMMIT details: file_size=%d file_type=%d", updatedFile.Size, wccAfter.Type)
	} else {
		logger.Warn("COMMIT: successful but cannot get updated file attributes: handle=%x error=%v",
			req.Handle, getErr)
	}

	logger.Info("COMMIT successful: file=%s offset=%d count=%d client=%s",
		file.ContentID, req.Offset, req.Count, clientIP)
	return &CommitResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		AttrBefore:      wccBefore,
		AttrAfter:       wccAfter,
		WriteVerifier:   serverBootTime,
	}, nil
}

// ============================================================================
// Commit Helper Functions
// ============================================================================

// flushCacheToContentStore flushes the cache to the content store.
//
// Strategy depends on content store capabilities:
//   - IncrementalWriteStore (S3): streaming multipart uploads
//   - WriteAtStore (filesystem, memory): write only new bytes at offset
//   - Basic store: write entire file (inefficient fallback)
//
// State transitions are handled by the background flusher, not here.
func flushCacheToContentStore(
	ctx *NFSHandlerContext,
	h *Handler,
	c cache.Cache,
	contentStore content.ContentStore,
	file *metadata.File,
	req *CommitRequest,
) error {
	contentID := file.ContentID
	cacheSize := c.Size(contentID)
	flushedOffset := c.GetFlushedOffset(contentID)

	// Check for incremental write support first (S3)
	if incStore, ok := contentStore.(content.IncrementalWriteStore); ok {
		// Incremental write (S3): parallel multipart uploads
		// No handler-level lock needed - FlushIncremental handles concurrency internally
		// using uploadedParts/uploadingParts maps to coordinate parallel uploads

		// Flush to content store - uploads complete parts in parallel
		// Returns 0 for small files (< partSize) - they'll use PutObject on finalization
		flushed, err := incStore.FlushIncremental(ctx.Context, contentID, c)
		if err != nil {
			return fmt.Errorf("incremental flush error: %w", err)
		}

		// Transition to StateUploading so the background flusher can complete the upload
		// when the file becomes idle (no more writes for flush_timeout duration)
		c.SetState(contentID, cache.StateUploading)

		logger.Info("COMMIT: flushed %d bytes incrementally: content_id=%s", flushed, contentID)
		return nil
	}

	// WriteAt-capable store (filesystem, memory): write only new bytes
	bytesToFlush := cacheSize - flushedOffset
	if bytesToFlush <= 0 {
		logger.Info("COMMIT: flushed 0 bytes (already up to date): content_id=%s", contentID)
		return nil
	}

	buf := make([]byte, bytesToFlush)
	n, err := c.ReadAt(ctx.Context, contentID, buf, flushedOffset)
	if err != nil && err != io.EOF {
		return fmt.Errorf("cache read error: %w", err)
	}

	err = contentStore.WriteAt(ctx.Context, contentID, buf[:n], flushedOffset)
	if err != nil {
		return fmt.Errorf("content store write error: %w", err)
	}

	c.SetFlushedOffset(contentID, flushedOffset+uint64(n))

	// Transition to StateUploading so the background flusher can finalize
	c.SetState(contentID, cache.StateUploading)

	logger.Info("COMMIT: flushed %d bytes at offset %d: content_id=%s", n, flushedOffset, contentID)

	return nil
}

// ============================================================================
// Request Validation
// ============================================================================

// commitValidationError represents a COMMIT request validation error.
type commitValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *commitValidationError) Error() string {
	return e.message
}

// validateCommitRequest validates COMMIT request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - Handle is at least 8 bytes (for file ID extraction)
//   - Offset + Count doesn't overflow uint64
//
// Returns:
//   - nil if valid
//   - *commitValidationError with NFS status if invalid
func validateCommitRequest(req *CommitRequest) *commitValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &commitValidationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.Handle) > 64 {
		return &commitValidationError{
			message:   fmt.Sprintf("handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &commitValidationError{
			message:   fmt.Sprintf("handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate offset + count doesn't overflow
	// This prevents potential integer overflow attacks
	if req.Count > 0 {
		if req.Offset > ^uint64(0)-uint64(req.Count) {
			return &commitValidationError{
				message:   fmt.Sprintf("offset + count overflow: offset=%d count=%d", req.Offset, req.Count),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeCommitRequest decodes a COMMIT request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.21 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the COMMIT request
//
// Returns:
//   - *CommitRequest: The decoded request containing handle, offset, and count
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded COMMIT request from network
//	req, err := DecodeCommitRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Count in COMMIT procedure
func DecodeCommitRequest(data []byte) (*CommitRequest, error) {
	// Validate minimum data length (4 bytes handle len + at least 1 byte handle + 8 bytes offset + 4 bytes count)
	if len(data) < 17 {
		return nil, fmt.Errorf("data too short: need at least 17 bytes, got %d", len(data))
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

	// Validate handle length (NFS v3 handles are typically <= 64 bytes per RFC 1813)
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	// Prevent zero-length handles
	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Read handle
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			break // tolerate missing padding bytes, as in DecodeWriteRequest
		}
	}

	// ========================================================================
	// Decode offset
	// ========================================================================

	// Read offset (8 bytes, big-endian)
	var offset uint64
	if err := binary.Read(reader, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("failed to read offset: %w", err)
	}

	// ========================================================================
	// Decode count
	// ========================================================================

	// Read count (4 bytes, big-endian)
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("failed to read count: %w", err)
	}

	logger.Debug("Decoded COMMIT request: handle_len=%d offset=%d count=%d",
		handleLen, offset, count)

	return &CommitRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
	}, nil
}

// ============================================================================
// Per-File Locking (Race Condition Prevention)
// ============================================================================

// acquireFileLock obtains a per-ContentID mutex to serialize COMMIT operations
// on the same file. This prevents race conditions when multiple concurrent
// COMMIT operations target the same file (e.g., due to parallel NFS requests).
//
// The lock is stored in h.fileLocks and must be released by calling releaseFileLock.
//
// Parameters:
//   - id: Content identifier to lock
//
// Returns:
//   - *sync.Mutex: The acquired lock (caller must unlock when done)
func (h *Handler) acquireFileLock(id metadata.ContentID) *sync.Mutex {
	// Get or create mutex for this content ID
	value, _ := h.fileLocks.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu
}

// releaseFileLock releases a per-ContentID mutex after COMMIT operations complete.
//
// This allows other concurrent COMMIT operations for the same file to proceed.
// The mutex is kept in the map for future operations (not deleted) to avoid
// allocation overhead.
//
// Parameters:
//   - mu: The mutex to unlock (obtained from acquireFileLock)
func (h *Handler) releaseFileLock(mu *sync.Mutex) {
	mu.Unlock()
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the CommitResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.21 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. File WCC data (always present):
//     a. Pre-op attributes (present flag + attributes if present)
//     b. Post-op attributes (present flag + attributes if present)
//  3. Write verifier (8 bytes, big-endian uint64) - only on success
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
//	resp := &CommitResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    AttrBefore:    wccBefore,
//	    AttrAfter:     wccAfter,
//	    WriteVerifier: 0,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *CommitResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write file WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the file.

	if err := xdr.EncodeWccData(&buf, resp.AttrBefore, resp.AttrAfter); err != nil {
		return nil, fmt.Errorf("failed to encode file wcc data: %w", err)
	}

	// ========================================================================
	// Write write verifier (only on success)
	// ========================================================================

	if resp.Status == types.NFS3OK {
		if err := binary.Write(&buf, binary.BigEndian, resp.WriteVerifier); err != nil {
			return nil, fmt.Errorf("failed to write verifier: %w", err)
		}
	}

	logger.Debug("Encoded COMMIT response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}
