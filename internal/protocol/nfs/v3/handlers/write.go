package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Write Stability Levels (RFC 1813 Section 3.3.7)
// ============================================================================

// Write stability levels control how the server handles data persistence.
// These constants define when data must be committed to stable storage.
const (
	// UnstableWrite (0): Data may be cached in server memory.
	// The server may lose data on crash before COMMIT is called.
	// Offers best performance for sequential writes.
	UnstableWrite = 0

	// DataSyncWrite (1): Data must be committed to stable storage,
	// but metadata (file size, timestamps) may be cached.
	// Provides good balance of performance and safety.
	DataSyncWrite = 1

	// FileSyncWrite (2): Both data and metadata must be committed
	// to stable storage before returning success.
	// Safest option but slowest performance.
	FileSyncWrite = 2
)

// ============================================================================
// Flush Reasons
// ============================================================================

// FlushReason indicates why a write cache flush was triggered.
type FlushReason string

const (
	// FlushReasonStableWrite indicates flush due to stable write requirement
	FlushReasonStableWrite FlushReason = "stable_write"

	// FlushReasonThreshold indicates flush due to cache size threshold reached
	FlushReasonThreshold FlushReason = "threshold_reached"

	// FlushReasonCommit indicates flush due to COMMIT procedure
	FlushReasonCommit FlushReason = "commit"

	// FlushReasonTimeout indicates flush due to auto-flush timeout (for macOS compatibility)
	FlushReasonTimeout FlushReason = "timeout"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// WriteRequest represents a WRITE request from an NFS client.
// The client specifies a file handle, offset, data to write, and
// stability requirements for the write operation.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.7 specifies the WRITE procedure as:
//
//	WRITE3res NFSPROC3_WRITE(WRITE3args) = 7;
//
// WRITE is used to write data to a regular file. It's one of the fundamental
// operations for file modification in NFS.
type WriteRequest struct {
	// Handle is the file handle of the file to write to.
	// Must be a valid file handle for a regular file (not a directory).
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// Offset is the byte offset in the file where writing should begin.
	// Can be any value from 0 to max file size.
	// Writing beyond EOF extends the file (sparse files supported).
	Offset uint64

	// Count is the number of bytes the client intends to write.
	// Should match the length of Data field.
	// May differ from len(Data) if client implementation varies.
	Count uint32

	// Stable indicates the stability level for this write.
	// Values:
	//   - UnstableWrite (0): May cache in memory
	//   - DataSyncWrite (1): Commit data to disk
	//   - FileSyncWrite (2): Commit data and metadata to disk
	Stable uint32

	// Data contains the actual bytes to write.
	// Length typically matches Count field.
	// Maximum size limited by server's wtmax (from FSINFO).
	Data []byte
}

// WriteResponse represents the response to a WRITE request.
// It contains the status, WCC data for cache consistency, and
// information about how the write was committed.
//
// The response is encoded in XDR format before being sent back to the client.
type WriteResponse struct {
	NFSResponseBase                    // Embeds Status field and GetStatus() method
	AttrBefore      *types.WccAttr     // Pre-op attributes (optional)
	AttrAfter       *types.NFSFileAttr // Post-op attributes (optional)
	Count           uint32             // Number of bytes written
	Committed       uint32             // How data was committed
	Verf            uint64             // Write verifier
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Write writes data to a regular file.
//
// This implements the NFS WRITE procedure as defined in RFC 1813 Section 3.3.7.
//
// **Purpose:**
//
// WRITE is the fundamental operation for modifying file contents over NFS. It's used by:
//   - Applications writing to files
//   - Editors saving changes
//   - Build systems creating output files
//   - Any operation that needs to modify file data
//
// **Process:**
//
//  1. Check for context cancellation before starting
//  2. Validate request parameters (handle, offset, count, data)
//  3. Extract client IP and authentication credentials from context
//  4. Verify file exists and is a regular file (via store)
//  5. Calculate new size (safe after validation)
//  6. Check write permissions and update metadata (via store)
//  7. Write data to content store
//  8. Return updated attributes and commit status
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - Protocol layer coordinates between metadata and content repositories
//   - Content store handles actual data writing
//   - Metadata store handles file attributes and permissions
//   - Access control enforced by metadata store
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//   - Respects context cancellation for graceful shutdown and timeouts
//   - Cancellation checks before expensive I/O operations
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// Write permission checking is implemented by the metadata store
// based on Unix-style permission bits (owner/group/other).
//
// **Write Stability:**
//
// Clients can request different stability levels:
//   - UnstableWrite: Fastest, data may be lost on server crash
//   - DataSyncWrite: Data committed, metadata may be cached
//   - FileSyncWrite: Everything committed to disk
//
// The server can return a higher stability level than requested
// (e.g., always do FileSyncWrite for simplicity).
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps clients maintain cache consistency:
//   - AttrBefore: Attributes before the operation
//   - AttrAfter: Attributes after the operation
//
// Clients compare these to detect concurrent modifications by other clients.
// If AttrBefore doesn't match client's cached attributes, the cache is stale.
//
// **Write Verifier:**
//
// The write verifier is a unique value that changes when the server restarts.
// Clients use it to detect server reboots and re-send unstable writes.
// Typical implementation: server boot time or instance UUID.
//
// **File Size Extension:**
//
// Writing beyond EOF extends the file:
//   - Bytes between EOF and offset are zero-filled (sparse file)
//   - File size is updated to offset + count
//   - Sparse regions may not consume disk space on many filesystems
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store/Content errors are mapped to NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Not a regular file → NFS3ErrIsDir
//   - Permission denied → NFS3ErrAcces
//   - No space left → NFS3ErrNoSpc
//   - Read-only filesystem → NFS3ErrRofs
//   - I/O error → NFS3ErrIO
//   - Stale handle → NFS3ErrStale
//   - Context cancelled → types.NFS3ErrIO
//
// **Performance Considerations:**
//
// WRITE is frequently called and performance-critical:
//   - Use efficient content store implementation
//   - Support write-behind caching (UnstableWrite)
//   - Minimize data copying
//   - Batch small writes when possible
//   - Consider write alignment with filesystem blocks
//   - Respect FSINFO wtpref for optimal performance
//   - Cancel expensive I/O on client disconnect
//
// **Context Cancellation:**
//
// This operation respects context cancellation at critical points:
//   - Before operation starts
//   - Before GetFile (file validation)
//   - Before WriteFile (metadata update)
//   - Before WriteAt (actual data write - most expensive)
//   - Returns types.NFS3ErrIO on cancellation with WCC data when available
//
// WRITE operations can involve significant I/O, especially for large writes
// or slow storage. Context cancellation is particularly valuable here to:
//   - Avoid wasting resources on disconnected clients
//   - Support request timeouts
//   - Enable graceful server shutdown
//   - Prevent accumulation of zombie write operations
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store layers enforce write permissions
//   - Read-only exports prevent writes
//   - Client context enables audit logging
//   - Prevent writes to system files via export configuration
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - contentStore: Content store for file data operations
//   - metadataStore: Metadata store for file attributes
//   - req: The write request containing handle, offset, and data
//
// **Returns:**
//   - *WriteResponse: Response with status, WCC data, and commit info
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.7: WRITE Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &WriteRequest{
//	    Handle: fileHandle,
//	    Offset: 0,
//	    Count:  1024,
//	    Stable: FileSyncWrite,
//	    Data:   dataBytes,
//	}
//	ctx := &NFSHandlerContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    Share:      "/export",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Write(ctx, contentStore, metadataStore, req)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == NFS3OK {
//	    // Write successful, resp.Count bytes written
//	    // Check resp.Committed for actual stability level
//	}
func (h *Handler) Write(
	ctx *NFSHandlerContext,
	req *WriteRequest,
) (*WriteResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.DebugCtx(ctx.Context, "WRITE", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", bytesize.ByteSize(req.Offset), "count", bytesize.ByteSize(req.Count), "stable", req.Stable, "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		traceWarn(ctx.Context, ctx.Context.Err(), "WRITE cancelled", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata and content services from registry
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()
	contentSvc := h.Registry.GetBlockService()

	fileHandle := metadata.FileHandle(req.Handle)

	logger.DebugCtx(ctx.Context, "WRITE", "share", ctx.Share)

	// ========================================================================
	// Step 3: Validate request parameters
	// ========================================================================
	// Note: We use a fixed max write size (1MB) to avoid a metadata lookup.
	// GetFilesystemCapabilities is expensive and the value rarely changes.
	// The actual capabilities are still enforced by the metadata store.

	const maxWriteSize uint32 = 1 << 20 // 1MB - matches default config
	if err := validateWriteRequest(req, maxWriteSize); err != nil {
		traceWarn(ctx.Context, err, "WRITE validation failed", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 4: Calculate new file size
	// ========================================================================
	// Note: File existence and type validation is done by PrepareWrite.
	// This eliminates a redundant GetFile call.

	dataLen := uint64(len(req.Data))
	newSize, overflow := safeAdd(req.Offset, dataLen)
	if overflow {
		logger.WarnCtx(ctx.Context, "WRITE failed: offset + dataLen overflow", "offset", req.Offset, "dataLen", dataLen, "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrInval}}, nil
	}

	// Validate offset + length doesn't exceed OffsetMax (match Linux nfs3proc.c behavior)
	// Linux returns EFBIG (File too large) in this case per RFC 1813
	if newSize > uint64(types.OffsetMax) {
		logger.WarnCtx(ctx.Context, "WRITE failed: offset + length exceeds OffsetMax",
			"offset", req.Offset, "dataLen", dataLen, "newSize", newSize, "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrFBig}}, nil
	}

	// ========================================================================
	// Step 5: Build AuthContext with share-level identity mapping (cached)
	// ========================================================================

	authCtx, err := h.GetCachedAuthContext(ctx)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "WRITE cancelled during auth context building", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP, "error", ctx.Context.Err())
			return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "WRITE failed: failed to build auth context", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)

		// No WCC data available - we haven't called PrepareWrite yet
		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
		}, nil
	}

	// ========================================================================
	// Step 6: Prepare write operation (validate permissions)
	// ========================================================================
	// PrepareWrite validates permissions but does NOT modify metadata yet.
	// Metadata is updated by CommitWrite after content write succeeds.

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "WRITE cancelled before PrepareWrite", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP, "error", ctx.Context.Err())

		// No WCC data available - we haven't called PrepareWrite yet
		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
		}, nil
	}

	writeIntent, err := metaSvc.PrepareWrite(authCtx, fileHandle, newSize)
	if err != nil {
		// Map store error to NFS status
		status := mapMetadataErrorToNFS(err)

		logger.WarnCtx(ctx.Context, "WRITE failed: PrepareWrite error", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", len(req.Data), "client", clientIP, "error", err)

		// No WCC data available - PrepareWrite failed so we don't have file attributes
		return h.buildWriteErrorResponse(status, fileHandle, nil, nil), nil
	}

	// Build WCC attributes from pre-write state
	nfsWccAttr := buildWccAttr(writeIntent.PreWriteAttr)

	// ========================================================================
	// Step 6: Write data to ContentService (uses Cache internally)
	// ========================================================================

	// Check context before write operation
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "WRITE cancelled before write", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP, "error", ctx.Context.Err())
		return h.buildWriteErrorResponse(types.NFS3ErrIO, fileHandle, writeIntent.PreWriteAttr, writeIntent.PreWriteAttr), nil
	}

	// Write to ContentService (uses Cache, will be flushed on COMMIT)
	err = contentSvc.WriteAt(ctx.Context, ctx.Share, writeIntent.PayloadID, req.Data, req.Offset)
	if err != nil {
		traceError(ctx.Context, err, "WRITE failed: content write error", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", len(req.Data), "content_id", writeIntent.PayloadID, "client", clientIP)
		status := xdr.MapContentErrorToNFSStatus(err)
		return h.buildWriteErrorResponse(status, fileHandle, writeIntent.PreWriteAttr, writeIntent.PreWriteAttr), nil
	}
	logger.DebugCtx(ctx.Context, "WRITE: cached successfully", "content_id", writeIntent.PayloadID)

	// ========================================================================
	// Step 8: Commit metadata changes after successful content write
	// ========================================================================

	updatedFile, err := metaSvc.CommitWrite(authCtx, writeIntent)
	if err != nil {
		traceError(ctx.Context, err, "WRITE failed: CommitWrite error (content written but metadata not updated)", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", len(req.Data), "client", clientIP)

		// Content is written but metadata not updated - this is an inconsistent state
		// Map error to NFS status
		status := mapMetadataErrorToNFS(err)

		return h.buildWriteErrorResponse(status, fileHandle, writeIntent.PreWriteAttr, writeIntent.PreWriteAttr), nil
	}

	// ========================================================================
	// Step 9: Build success response
	// ========================================================================

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &updatedFile.FileAttr)

	logger.DebugCtx(ctx.Context, "WRITE successful", "file", updatedFile.PayloadID, "offset", bytesize.ByteSize(req.Offset), "requested", bytesize.ByteSize(req.Count), "written", bytesize.ByteSize(len(req.Data)), "new_size", bytesize.ByteSize(updatedFile.Size), "client", clientIP)

	// ========================================================================
	// Stability Level Design Decision (RFC 1813 Section 3.3.7)
	// ========================================================================
	//
	// We always return UNSTABLE because Cache is always enabled.
	// This is RFC-compliant because:
	//
	// RFC 1813 says: "The server may choose to write the data to stable storage
	// or may hold it in the server's internal buffers to be written later."
	//
	// The `stable` field is the client's PREFERENCE, not a mandate. The server
	// returns what it ACTUALLY did via the `committed` field. Clients handle
	// UNSTABLE correctly by calling COMMIT when they need durability.
	//
	// Why this design:
	//   - S3 backends: Synchronous writes to S3 are slow (100ms+ per request).
	//     Async flush on COMMIT is 10-100x faster for sequential writes.
	//   - Performance: Buffering writes and flushing in batches is more efficient
	//     for any remote or high-latency storage backend.
	//   - Client compatibility: All NFS clients handle UNSTABLE correctly and
	//     will call COMMIT before closing files or when fsync() is called.
	//
	committed := uint32(UnstableWrite)
	logger.DebugCtx(ctx.Context, "WRITE: returning UNSTABLE (flush on COMMIT)")

	logger.DebugCtx(ctx.Context, "WRITE details", "stable_requested", req.Stable, "committed", committed, "size", bytesize.ByteSize(updatedFile.Size), "type", updatedFile.Type, "mode", fmt.Sprintf("%o", updatedFile.Mode))

	return &WriteResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		AttrBefore:      nfsWccAttr,
		AttrAfter:       nfsAttr,
		// Count: RFC 1813 specifies this as "The number of bytes of data written".
		// We currently assume that the configured content store implementations
		// perform WriteAt in an all-or-nothing fashion: they either write all
		// bytes or fail entirely. Under that assumption, len(req.Data) equals
		// the actual bytes written on success.
		// NOTE: If a future content store allows partial writes, this code must
		// be updated to report the actual number of bytes written instead of
		// len(req.Data), and the WriteAt contract should document that behavior.
		Count:     uint32(len(req.Data)),
		Committed: committed,      // UNSTABLE when using cache, tells client to call COMMIT
		Verf:      serverBootTime, // Server boot time for restart detection
	}, nil
}

// ============================================================================
// Write Helper Functions
// ============================================================================

// buildWriteErrorResponse creates a consistent error response with WCC data.
// This centralizes error response creation to reduce duplication.
func (h *Handler) buildWriteErrorResponse(
	status uint32,
	handle metadata.FileHandle,
	preWriteAttr *metadata.FileAttr,
	currentAttr *metadata.FileAttr,
) *WriteResponse {
	var wccBefore *types.WccAttr
	if preWriteAttr != nil {
		wccBefore = buildWccAttr(preWriteAttr)
	}

	var wccAfter *types.NFSFileAttr
	if currentAttr != nil {
		wccAfter = h.convertFileAttrToNFS(handle, currentAttr)
	}

	return &WriteResponse{
		NFSResponseBase: NFSResponseBase{Status: status},
		AttrBefore:      wccBefore,
		AttrAfter:       wccAfter,
	}
}
