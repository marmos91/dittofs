package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
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
//   - File handle validation performed by metaSvc.GetFile()
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

	logger.InfoCtx(ctx.Context, "COMMIT", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "COMMIT cancelled", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP, "error", ctx.Context.Err())
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateCommitRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "COMMIT validation failed", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP, "error", err)
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 3: Get metadata service
	// ========================================================================

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "COMMIT failed: metadata service not initialized", "client", clientIP, "error", err)
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	handle := metadata.FileHandle(req.Handle)

	// ========================================================================
	// Step 4: Verify file exists and capture pre-operation state
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "COMMIT cancelled before GetFile", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP, "error", ctx.Context.Err())
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	file, err := metaSvc.GetFile(ctx.Context, handle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "COMMIT failed: file not found", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP, "error", err)
		return &CommitResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(&file.FileAttr)

	// Verify this is not a directory
	if file.Type == metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "COMMIT failed: handle is a directory", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)

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
		logger.WarnCtx(ctx.Context, "COMMIT cancelled before flush", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP, "error", ctx.Context.Err())

		// Get updated attributes for WCC data (best effort)
		var wccAfter *types.NFSFileAttr
		if updatedFile, getErr := metaSvc.GetFile(ctx.Context, handle); getErr == nil {
			wccAfter = h.convertFileAttrToNFS(handle, &updatedFile.FileAttr)
		}

		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, nil
	}

	// Convert file attributes for WCC "after" data
	wccAfter := h.convertFileAttrToNFS(handle, &file.FileAttr)

	// Check if there's content to flush
	if file.PayloadID == "" {
		logger.DebugCtx(ctx.Context, "COMMIT: no content to flush")
		logger.InfoCtx(ctx.Context, "COMMIT successful (no content)", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP)
		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
			WriteVerifier:   serverBootTime,
		}, nil
	}

	// ========================================================================
	// Step 5: Flush data to ContentService (uses Cache internally)
	// ========================================================================

	logger.InfoCtx(ctx.Context, "COMMIT: flushing data", "share", ctx.Share)

	payloadSvc, err := getPayloadService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "COMMIT failed: payload service not initialized", "client", clientIP, "error", err)
		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, nil
	}

	// Flush cache to content store using ContentService (non-blocking)
	//
	// NOTE: Flush is non-blocking because:
	//   - Data is already safe in WAL-backed cache (crash-safe)
	//   - macOS sends COMMIT every 4MB, so blocking would kill performance
	//   - Background uploads happen via eager upload mechanism
	//   - This matches the NFS3 spec which allows async commits
	//
	// Per RFC 1813 Section 3.3.21, the server MAY choose when data reaches
	// stable storage. Our WAL cache provides the durability guarantee.
	_, flushErr := payloadSvc.Flush(ctx.Context, file.PayloadID)
	if flushErr != nil {
		traceError(ctx.Context, flushErr, "COMMIT failed: flush error", "handle", fmt.Sprintf("0x%x", req.Handle), "content_id", file.PayloadID, "client", clientIP)

		// Try to get updated attributes for error response
		if updatedFile, getErr := metaSvc.GetFile(ctx.Context, handle); getErr == nil {
			wccAfter = h.convertFileAttrToNFS(handle, &updatedFile.FileAttr)
		}

		return &CommitResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Flush pending metadata writes (deferred commit optimization)
	// ========================================================================

	// Build auth context for metadata flush
	authCtx, authErr := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if authErr == nil {
		// Flush pending metadata for this specific file
		flushed, metaErr := metaSvc.FlushPendingWriteForFile(authCtx, handle)
		if metaErr != nil {
			logger.WarnCtx(ctx.Context, "COMMIT: metadata flush failed", "handle", fmt.Sprintf("0x%x", req.Handle), "error", metaErr)
			// Continue - content is flushed, metadata will be fixed eventually
		} else if flushed {
			logger.DebugCtx(ctx.Context, "COMMIT: flushed pending metadata", "handle", fmt.Sprintf("0x%x", req.Handle))
		}
	}

	// Get updated file attributes for WCC data (file may have changed after flush)
	if updatedFile, getErr := metaSvc.GetFile(ctx.Context, handle); getErr == nil {
		wccAfter = h.convertFileAttrToNFS(handle, &updatedFile.FileAttr)
		logger.DebugCtx(ctx.Context, "COMMIT details", "file_size", updatedFile.Size, "file_type", wccAfter.Type)
	} else {
		logger.WarnCtx(ctx.Context, "COMMIT: successful but cannot get updated file attributes", "handle", fmt.Sprintf("0x%x", req.Handle), "error", getErr)
	}

	logger.InfoCtx(ctx.Context, "COMMIT successful", "file", file.PayloadID, "offset", req.Offset, "count", req.Count, "client", clientIP)
	return &CommitResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		AttrBefore:      wccBefore,
		AttrAfter:       wccAfter,
		WriteVerifier:   serverBootTime,
	}, nil
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
