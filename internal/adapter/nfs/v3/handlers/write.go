package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
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

// Write handles NFS WRITE (RFC 1813 Section 3.3.7).
// Writes data to a regular file at a given offset using two-phase PrepareWrite/CommitWrite pattern.
// Delegates to MetadataService.PrepareWrite+CommitWrite and PayloadService.WriteAt (cache-backed).
// Updates file size/timestamps via metadata; writes data to cache (flushed on COMMIT); returns WCC data.
// Errors: NFS3ErrNoEnt, NFS3ErrAcces, NFS3ErrFBig (offset overflow), NFS3ErrNoSpc, NFS3ErrIO.
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
		logWarn(ctx.Context, ctx.Context.Err(), "WRITE cancelled", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata and content services from registry
	// ========================================================================

	metaSvc, payloadSvc, err := getServices(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "WRITE failed: service not initialized", "client", clientIP, "error", err)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

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
		logWarn(ctx.Context, err, "WRITE validation failed", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)
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

		logError(ctx.Context, err, "WRITE failed: failed to build auth context", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)

		// No WCC data available - we haven't called PrepareWrite yet
		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
		}, nil
	}

	// ========================================================================
	// Step 5b: Cross-protocol oplock break (placeholder)
	// ========================================================================
	// TODO(plan-03): Wire to LockManager.CheckAndBreakOpLocksForWrite() once
	// centralized break methods are available (Phase 26 Plan 03).
	// Previously: metaSvc.CheckAndBreakLeasesForWrite(ctx, handle)

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
	err = payloadSvc.WriteAt(ctx.Context, writeIntent.PayloadID, req.Data, req.Offset)
	if err != nil {
		logError(ctx.Context, err, "WRITE failed: content write error", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", len(req.Data), "content_id", writeIntent.PayloadID, "client", clientIP)
		status := xdr.MapContentErrorToNFSStatus(err)
		return h.buildWriteErrorResponse(status, fileHandle, writeIntent.PreWriteAttr, writeIntent.PreWriteAttr), nil
	}
	logger.DebugCtx(ctx.Context, "WRITE: cached successfully", "content_id", writeIntent.PayloadID)

	// ========================================================================
	// Step 8: Commit metadata changes after successful content write
	// ========================================================================

	updatedFile, err := metaSvc.CommitWrite(authCtx, writeIntent)
	if err != nil {
		logError(ctx.Context, err, "WRITE failed: CommitWrite error (content written but metadata not updated)", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", len(req.Data), "client", clientIP)

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

// Cross-protocol oplock break function removed (Phase 26 Plan 04).
// Will be replaced by LockManager.CheckAndBreakOpLocksFor{Write,Read}()
// once centralized break methods are available (Phase 26 Plan 03).
