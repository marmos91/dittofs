package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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

	logger.Info("WRITE: handle=%x offset=%d count=%d stable=%d client=%s auth=%d",
		req.Handle, req.Offset, req.Count, req.Stable, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.Warn("WRITE cancelled: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, ctx.Context.Err())
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata and content stores from context
	// ========================================================================

	metadataStore, err := h.getMetadataStore(ctx)
	if err != nil {
		logger.Warn("WRITE failed: %v handle=%x client=%s", err, req.Handle, clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	// Get content store for this share
	contentStore, err := h.getContentStore(ctx)
	if err != nil {
		logger.Warn("WRITE failed: %v handle=%x client=%s", err, req.Handle, clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Get write cache for this share (optional - may be nil for sync mode)
	writeCache, err := h.Registry.GetWriteCacheForShare(ctx.Share)
	if err != nil {
		logger.Error("WRITE failed: cannot get write cache: share=%s handle=%x client=%s error=%v",
			ctx.Share, req.Handle, clientIP, err)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	if writeCache != nil {
		logger.Debug("WRITE: share=%s mode=async (using write cache)", ctx.Share)
	} else {
		logger.Debug("WRITE: share=%s mode=sync (no write cache)", ctx.Share)
	}

	// ========================================================================
	// Step 3: Validate request parameters
	// ========================================================================

	caps, err := metadataStore.GetFilesystemCapabilities(ctx.Context, fileHandle)
	if err != nil {
		logger.Warn("WRITE failed: cannot get capabilities: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	if err := validateWriteRequest(req, caps.MaxWriteSize); err != nil {
		logger.Warn("WRITE validation failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 3: Verify file exists and is a regular file
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.Warn("WRITE cancelled before GetFile: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, ctx.Context.Err())
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	file, status, err := h.getFileOrError(ctx, metadataStore, fileHandle, "WRITE", req.Handle)
	if file == nil {
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Verify it's a regular file (not a directory or special file)
	if file.Type != metadata.FileTypeRegular {
		logger.Warn("WRITE failed: not a regular file: handle=%x type=%d client=%s",
			req.Handle, file.Type, clientIP)

		// Return file attributes even on error for cache consistency
		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIsDir}, // NFS3ErrIsDir used for all non-regular files
			AttrAfter:       nfsAttr,
		}, nil
	}

	// ========================================================================
	// Step 4: Calculate new file size
	// ========================================================================

	dataLen := uint64(len(req.Data))
	newSize, overflow := safeAdd(req.Offset, dataLen)
	if overflow {
		logger.Warn("WRITE failed: offset + dataLen overflow: offset=%d dataLen=%d client=%s",
			req.Offset, dataLen, clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrInval}}, nil
	}

	// ========================================================================
	// Step 5: Build AuthContext with share-level identity mapping
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("WRITE cancelled during auth context building: handle=%x client=%s error=%v",
				req.Handle, clientIP, ctx.Context.Err())
			return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		logger.Error("WRITE failed: failed to build auth context: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)

		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrAfter:       nfsAttr,
		}, nil
	}

	// ========================================================================
	// Step 6: Prepare write operation (validate permissions)
	// ========================================================================
	// PrepareWrite validates permissions but does NOT modify metadata yet.
	// Metadata is updated by CommitWrite after content write succeeds.

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.Warn("WRITE cancelled before PrepareWrite: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, ctx.Context.Err())

		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrAfter:       nfsAttr,
		}, nil
	}

	writeIntent, err := metadataStore.PrepareWrite(authCtx, fileHandle, newSize)
	if err != nil {
		// Map store error to NFS status
		status := mapMetadataErrorToNFS(err)

		logger.Warn("WRITE failed: PrepareWrite error: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, len(req.Data), clientIP, err)

		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		// Build WCC attributes from current state
		nfsWccAttr := &types.WccAttr{
			Size: file.Size,
			Mtime: types.TimeVal{
				Seconds:  uint32(file.Mtime.Unix()),
				Nseconds: uint32(file.Mtime.Nanosecond()),
			},
			Ctime: types.TimeVal{
				Seconds:  uint32(file.Ctime.Unix()),
				Nseconds: uint32(file.Ctime.Nanosecond()),
			},
		}

		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			AttrBefore:      nfsWccAttr,
			AttrAfter:       nfsAttr,
		}, nil
	}

	// Build WCC attributes from pre-write state
	nfsWccAttr := &types.WccAttr{
		Size: writeIntent.PreWriteAttr.Size,
		Mtime: types.TimeVal{
			Seconds:  uint32(writeIntent.PreWriteAttr.Mtime.Unix()),
			Nseconds: uint32(writeIntent.PreWriteAttr.Mtime.Nanosecond()),
		},
		Ctime: types.TimeVal{
			Seconds:  uint32(writeIntent.PreWriteAttr.Ctime.Unix()),
			Nseconds: uint32(writeIntent.PreWriteAttr.Ctime.Nanosecond()),
		},
	}

	// ========================================================================
	// Step 6: Write data to cache or content store
	// ========================================================================
	// Write modes:
	//   1. Cached mode (if WriteCache available): Write to cache first
	//   2. Direct mode (no cache): Write directly to content store

	// Check context before write operation
	if ctx.isContextCancelled() {
		logger.Warn("WRITE cancelled before write: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, ctx.Context.Err())

		nfsAttr := h.convertFileAttrToNFS(fileHandle, writeIntent.PreWriteAttr)

		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrBefore:      nfsWccAttr,
			AttrAfter:       nfsAttr,
		}, nil
	}

	if writeCache != nil {
		// CACHED MODE: Write to cache
		logger.Debug("WRITE: cached mode: content_id=%s offset=%d count=%d",
			writeIntent.ContentID, req.Offset, len(req.Data))

		err = writeCache.WriteAt(ctx.Context, writeIntent.ContentID, req.Data, int64(req.Offset))
		if err != nil {
			logger.Error("WRITE failed: cache write error: handle=%x offset=%d count=%d content_id=%s client=%s error=%v",
				req.Handle, req.Offset, len(req.Data), writeIntent.ContentID, clientIP, err)

			nfsAttr := h.convertFileAttrToNFS(fileHandle, writeIntent.PreWriteAttr)

			return &WriteResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				AttrBefore:      nfsWccAttr,
				AttrAfter:       nfsAttr,
			}, nil
		}

		logger.Debug("WRITE: cached successfully: content_id=%s cache_size=%d",
			writeIntent.ContentID, writeCache.Size(writeIntent.ContentID))
	} else {
		// DIRECT MODE: Write directly to content store
		logger.Debug("WRITE: direct mode: content_id=%s offset=%d count=%d",
			writeIntent.ContentID, req.Offset, len(req.Data))

		err = contentStore.WriteAt(ctx.Context, writeIntent.ContentID, req.Data, int64(req.Offset))
		if err != nil {
			logger.Error("WRITE failed: content write error: handle=%x offset=%d count=%d content_id=%s client=%s error=%v",
				req.Handle, req.Offset, len(req.Data), writeIntent.ContentID, clientIP, err)

			nfsAttr := h.convertFileAttrToNFS(fileHandle, writeIntent.PreWriteAttr)

			status := xdr.MapContentErrorToNFSStatus(err)

			return &WriteResponse{
				NFSResponseBase: NFSResponseBase{Status: status},
				AttrBefore:      nfsWccAttr,
				AttrAfter:       nfsAttr,
			}, nil
		}

		logger.Debug("WRITE: direct write successful: content_id=%s", writeIntent.ContentID)
	}

	// ========================================================================
	// Step 7.5: Populate read cache after successful direct write (no write cache)
	// ========================================================================
	// If we wrote directly to content store (no write cache), populate the read cache
	// This makes the newly written data available for fast subsequent reads
	if writeCache == nil {
		readCache, rcErr := h.Registry.GetReadCacheForShare(ctx.Share)
		if rcErr != nil {
			logger.Warn("WRITE: cannot get read cache: share=%s error=%v", ctx.Share, rcErr)
		} else if readCache != nil && len(req.Data) > 0 {
			// Only cache small to medium files (< 10MB) to avoid thrashing
			const maxReadCacheSize = 10 * 1024 * 1024 // 10MB
			if newSize <= uint64(maxReadCacheSize) {
				// Write the data we just wrote at the appropriate offset
				rcErr = readCache.WriteAt(ctx.Context, writeIntent.ContentID, req.Data, int64(req.Offset))
				if rcErr != nil {
					// Cache population failure is not fatal - log and continue
					logger.Warn("WRITE: failed to populate read cache: handle=%x content_id=%s offset=%d size=%d error=%v",
						req.Handle, writeIntent.ContentID, req.Offset, len(req.Data), rcErr)
				} else {
					logger.Debug("WRITE: populated read cache: handle=%x content_id=%s offset=%d size=%d cache_size=%d",
						req.Handle, writeIntent.ContentID, req.Offset, len(req.Data), readCache.Size(writeIntent.ContentID))
				}
			} else {
				logger.Debug("WRITE: skipping read cache population for large file: handle=%x size=%d max_cache_size=%d",
					req.Handle, newSize, maxReadCacheSize)
			}
		}
	}

	// ========================================================================
	// Step 8: Commit metadata changes after successful content write
	// ========================================================================

	updatedFile, err := metadataStore.CommitWrite(authCtx, writeIntent)
	if err != nil {
		logger.Error("WRITE failed: CommitWrite error (content written but metadata not updated): handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, len(req.Data), clientIP, err)

		// Content is written but metadata not updated - this is an inconsistent state
		// Map error to NFS status
		status := mapMetadataErrorToNFS(err)

		nfsAttr := h.convertFileAttrToNFS(fileHandle, writeIntent.PreWriteAttr)

		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			AttrBefore:      nfsWccAttr,
			AttrAfter:       nfsAttr,
		}, nil
	}

	// ========================================================================
	// Step 9: Build success response
	// ========================================================================

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &updatedFile.FileAttr)

	logger.Info("WRITE successful: handle=%x offset=%d requested=%d written=%d new_size=%d client=%s",
		req.Handle, req.Offset, req.Count, len(req.Data), updatedFile.Size, clientIP)

	// Determine what stability level to return based on whether we're using a cache
	//
	// WITH cache:
	//   - Return UNSTABLE to tell client data is buffered
	//   - Client will call COMMIT when it wants durability
	//   - This prevents flush-on-every-write performance issue
	//
	// WITHOUT cache (direct write to content store):
	//   - Return requested stability level (data is already committed)
	//   - Client won't call COMMIT since we said data is stable
	//   - No unnecessary COMMIT calls
	var committed uint32
	if writeCache != nil {
		// Cache enabled: always return UNSTABLE so client will call COMMIT
		committed = uint32(UnstableWrite)
		logger.Debug("WRITE: returning UNSTABLE (cache enabled, flush on COMMIT)")
	} else {
		// No cache: data written directly to storage, return requested stability
		committed = req.Stable
		logger.Debug("WRITE: returning stability=%d (no cache, data committed)", committed)
	}

	logger.Debug("WRITE details: stable_requested=%d committed=%d size=%d type=%d mode=%o",
		req.Stable, committed, updatedFile.Size, updatedFile.Type, updatedFile.Mode)

	return &WriteResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		AttrBefore:      nfsWccAttr,
		AttrAfter:       nfsAttr,
		Count:           uint32(len(req.Data)),
		Committed:       committed,      // UNSTABLE when using cache, tells client to call COMMIT
		Verf:            serverBootTime, // Server boot time for restart detection
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// writeValidationError represents a WRITE request validation error.
type writeValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *writeValidationError) Error() string {
	return e.message
}

// validateWriteRequest validates WRITE request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - File handle is long enough for file ID extraction
//   - Count matches actual data length
//   - Count doesn't exceed server's maximum write size
//   - Offset + Count doesn't overflow uint64
//   - Stability level is valid
//
// Parameters:
//   - req: The write request to validate
//   - maxWriteSize: Maximum write size from store configuration
//
// Returns:
//   - nil if valid
//   - *writeValidationError with NFS status if invalid
func validateWriteRequest(req *WriteRequest, maxWriteSize uint32) *writeValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &writeValidationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &writeValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &writeValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate data length matches count
	// Some tolerance is acceptable, but large mismatches indicate corruption
	dataLen := uint32(len(req.Data))
	if dataLen != req.Count {
		logger.Warn("WRITE: count mismatch: count=%d data_len=%d (proceeding with actual data length)",
			req.Count, dataLen)
		// Not fatal - we'll use actual data length
	}

	// Validate count doesn't exceed maximum write size configured by store
	// The store can configure this based on its constraints and the
	// wtmax value advertised in FSINFO.
	if dataLen > maxWriteSize {
		return &writeValidationError{
			message:   fmt.Sprintf("write data too large: %d bytes (max %d)", dataLen, maxWriteSize),
			nfsStatus: types.NFS3ErrFBig,
		}
	}

	// Validate offset + count doesn't overflow
	// This prevents integer overflow attacks
	// CRITICAL: This must be checked BEFORE any calculations use offset + count
	if req.Offset > ^uint64(0)-uint64(dataLen) {
		return &writeValidationError{
			message:   fmt.Sprintf("offset + count would overflow: offset=%d count=%d", req.Offset, dataLen),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate stability level
	if req.Stable > FileSyncWrite {
		return &writeValidationError{
			message:   fmt.Sprintf("invalid stability level: %d (max %d)", req.Stable, FileSyncWrite),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeWriteRequest decodes a WRITE request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.7 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//  6. Stable (4 bytes, big-endian uint32)
//  7. Data length (4 bytes, big-endian uint32)
//  8. Data bytes (variable length)
//  9. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the WRITE request
//
// Returns:
//   - *WriteRequest: The decoded request containing handle, offset, and data
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded WRITE request from network
//	req, err := DecodeWriteRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Data in WRITE procedure
func DecodeWriteRequest(data []byte) (*WriteRequest, error) {
	// Validate minimum data length
	// 4 bytes (handle length) + 8 bytes (offset) + 4 bytes (count) +
	// 4 bytes (stable) + 4 bytes (data length) = 24 bytes minimum
	if len(data) < 24 {
		return nil, fmt.Errorf("data too short: need at least 24 bytes, got %d", len(data))
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
		// XDR allows missing trailing padding, so tolerate missing bytes here.
		if _, err := reader.ReadByte(); err != nil {
			break
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

	// ========================================================================
	// Decode stability level
	// ========================================================================

	var stable uint32
	if err := binary.Read(reader, binary.BigEndian, &stable); err != nil {
		return nil, fmt.Errorf("failed to read stable: %w", err)
	}

	// ========================================================================
	// Decode data
	// ========================================================================

	// Read data length
	var dataLen uint32
	if err := binary.Read(reader, binary.BigEndian, &dataLen); err != nil {
		return nil, fmt.Errorf("failed to read data length: %w", err)
	}

	// Validate data length during decoding to prevent memory exhaustion.
	// This is a hard-coded safety limit to prevent allocating excessive
	// memory before we can even validate the request. The actual validation
	// will use the store's configured maximum.
	//
	// This limit (32MB) is chosen to be:
	//   - Large enough to accommodate any reasonable store configuration
	//   - Small enough to prevent memory exhaustion attacks during XDR decoding
	//   - Higher than typical NFS write sizes (64KB-1MB)
	const maxDecodingSize = 32 * 1024 * 1024 // 32MB
	if dataLen > maxDecodingSize {
		return nil, fmt.Errorf("data length too large: %d bytes (max %d for decoding)", dataLen, maxDecodingSize)
	}

	// ZERO-COPY OPTIMIZATION: Instead of allocating a new buffer, slice the
	// original data buffer. This avoids a memory allocation and copy operation.
	// The data remains valid until the pooled buffer is returned (after handler completes).
	//
	// Calculate current offset in the original data slice
	currentPos := len(data) - reader.Len()

	// Validate we have enough data remaining
	if currentPos+int(dataLen) > len(data) {
		return nil, fmt.Errorf("insufficient data: need %d bytes, have %d", dataLen, len(data)-currentPos)
	}

	// Slice the original buffer (zero-copy)
	writeData := data[currentPos : currentPos+int(dataLen)]

	// Advance the reader position to skip the data we just sliced
	if _, err := reader.Seek(int64(dataLen), io.SeekCurrent); err != nil {
		return nil, fmt.Errorf("failed to advance reader: %w", err)
	}

	// Skip padding to 4-byte boundary (XDR alignment requirement)
	dataPadding := (4 - (dataLen % 4)) % 4
	for i := uint32(0); i < dataPadding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			// Padding is optional at end of message, don't fail
			break
		}
	}

	logger.Debug("Decoded WRITE request: handle_len=%d offset=%d count=%d stable=%d data_len=%d",
		handleLen, offset, count, stable, dataLen)

	return &WriteRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
		Stable: stable,
		Data:   writeData,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the WriteResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.7 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. WCC data (weak cache consistency):
//     a. Pre-op attributes (present flag + wcc_attr if present)
//     b. Post-op attributes (present flag + file_attr if present)
//  3. If status == NFS3OK:
//     a. Count (4 bytes, big-endian uint32)
//     b. Committed (4 bytes, big-endian uint32)
//     c. Write verifier (8 bytes, big-endian uint64)
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
//	resp := &WriteResponse{
//	    NFSResponseBase: NFSResponseBase{Status: NFS3OK},
//	    AttrBefore: wccAttr,
//	    AttrAfter:  fileAttr,
//	    Count:      1024,
//	    Committed:  FileSyncWrite,
//	    Verf:       12345,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *WriteResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write WCC data (Weak Cache Consistency)
	// ========================================================================
	// WCC data is included in both success and error cases to help
	// clients maintain cache consistency.

	// Write pre-op attributes
	if resp.AttrBefore != nil {
		// Present flag = TRUE (1)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("failed to write pre-op present flag: %w", err)
		}

		// Write WCC attributes (size, mtime, ctime)
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Size); err != nil {
			return nil, fmt.Errorf("failed to write pre-op size: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Seconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op mtime seconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Nseconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op mtime nseconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Seconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op ctime seconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Nseconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op ctime nseconds: %w", err)
		}
	} else {
		// Present flag = FALSE (0)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("failed to write pre-op absent flag: %w", err)
		}
	}

	// Write post-op attributes
	if err := xdr.EncodeOptionalFileAttr(&buf, resp.AttrAfter); err != nil {
		return nil, fmt.Errorf("failed to encode post-op attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return early if status is not OK
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding WRITE error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write count, committed, and verifier
	// ========================================================================

	// Write count (number of bytes actually written)
	if err := binary.Write(&buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("failed to write count: %w", err)
	}

	// Write committed (stability level actually achieved)
	if err := binary.Write(&buf, binary.BigEndian, resp.Committed); err != nil {
		return nil, fmt.Errorf("failed to write committed: %w", err)
	}

	// Write verifier (8 bytes - server instance identifier)
	if err := binary.Write(&buf, binary.BigEndian, resp.Verf); err != nil {
		return nil, fmt.Errorf("failed to write verifier: %w", err)
	}

	logger.Debug("Encoded WRITE response: %d bytes total, status=%d count=%d committed=%d",
		buf.Len(), resp.Status, resp.Count, resp.Committed)

	return buf.Bytes(), nil
}
