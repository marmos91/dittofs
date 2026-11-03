package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Write Stability Levels (RFC 1813 Section 3.3.7)

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

// Flush Reasons

// FlushReason indicates why a block store flush was triggered.
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

// Request and Response Structures

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

// Protocol Handler

// Write handles NFS WRITE (RFC 1813 Section 3.3.7).
// Writes data to a regular file at a given offset using two-phase PrepareWrite/CommitWrite pattern.
// Delegates to MetadataService.PrepareWrite+CommitWrite and BlockStore.WriteAt (cache-backed).
// Updates file size/timestamps via metadata; writes data to local store (flushed on COMMIT); returns WCC data.
// Errors: NFS3ErrNoEnt, NFS3ErrAcces, NFS3ErrFBig (offset overflow), NFS3ErrNoSpc, NFS3ErrIO.
func (h *Handler) Write(
	ctx *NFSHandlerContext,
	req *WriteRequest,
) (*WriteResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.LazyClientIP(ctx.ClientAddr)

	logger.DebugCtx(ctx.Context, "WRITE", "handle", xdr.LazyHandle(req.Handle), "offset", bytesize.ByteSize(req.Offset), "count", bytesize.ByteSize(req.Count), "stable", req.Stable, "client", clientIP, "auth", ctx.AuthFlavor)

	if ctx.isContextCancelled() {
		logWarn(ctx.Context, ctx.Context.Err(), "WRITE cancelled", "handle", xdr.LazyHandle(req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Structural validation only (handle, overflow, stability). The size cap
	// is applied as a short-write below, derived from the advertised wtmax.

	if err := validateWriteRequest(req); err != nil {
		logWarn(ctx.Context, err, "WRITE validation failed", "handle", xdr.LazyHandle(req.Handle), "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "WRITE failed: service not available", "client", clientIP, "error", err)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	logger.DebugCtx(ctx.Context, "WRITE", "share", ctx.Share)

	// Derive the per-request cap from the same value advertised by FSINFO
	// (GetFilesystemCapabilities().MaxWriteSize / wtmax) so the WRITE limit can
	// never drift from what the client was told. RFC 1813 permits the server to
	// write fewer bytes than requested; an over-large request is short-written
	// (data truncated, reply Count reflects the actual bytes) rather than
	// rejected with NFS3ERR_FBIG. This mirrors Linux nfsd, which clamps to
	// svc_max_payload. A genuine "file too large" (offset overflow / OffsetMax)
	// still returns NFS3ERR_FBIG below.
	//
	// wtmax is a static store-level limit, so it is read from the process-wide
	// cache (populated by FSINFO) instead of a per-RPC GetFilesystemCapabilities
	// lookup. On a cold cache (WRITE before any FSINFO) we fetch once and
	// populate it; the common mount sequence (FSINFO precedes WRITE) keeps the
	// store off the hot WRITE path entirely.
	maxWriteSize := fsMaxWriteSize.Load()
	if maxWriteSize == 0 {
		if caps, capErr := metaSvc.GetFilesystemCapabilities(ctx.Context, fileHandle); capErr == nil && caps != nil {
			maxWriteSize = caps.MaxWriteSize
			cacheMax(&fsMaxWriteSize, maxWriteSize)
		}
	}
	if maxWriteSize > 0 && uint32(len(req.Data)) > maxWriteSize {
		logger.DebugCtx(ctx.Context, "WRITE: short-write to advertised wtmax",
			"requested", len(req.Data), "wtmax", maxWriteSize, "client", clientIP)
		req.Data = req.Data[:maxWriteSize]
		req.Count = maxWriteSize
	}

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

	authCtx, err := h.GetCachedAuthContext(ctx)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "WRITE cancelled during auth context building", "handle", xdr.LazyHandle(req.Handle), "client", clientIP, "error", ctx.Context.Err())
			return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		logAuthCtxError(ctx.Context, err, "WRITE", "handle", xdr.LazyHandle(req.Handle), "client", clientIP)

		// No WCC data available - we haven't called PrepareWrite yet
		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: authDenialStatus(err)},
		}, nil
	}

	// PrepareWrite validates the target (type, permissions, read-only share)
	// and modifies no metadata, so a refused write still answers with its own
	// status.
	writeIntent, err := metaSvc.PrepareWrite(authCtx, fileHandle, newSize)
	if err != nil {
		logger.WarnCtx(ctx.Context, "WRITE failed: PrepareWrite error", "handle", xdr.LazyHandle(req.Handle), "offset", req.Offset, "count", len(req.Data), "client", clientIP, "error", err)
		return h.buildWriteErrorResponse(types.StatusForErr(err), fileHandle, nil, nil), nil
	}

	// A zero-count write writes nothing, so it needs no content store: the
	// file is left as it is and any committed level is truthful.
	if len(req.Data) == 0 {
		wcc := xdr.CaptureWccAttr(writeIntent.PreWriteAttr)
		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			AttrBefore:      wcc,
			AttrAfter:       h.convertFileAttrToNFS(fileHandle, writeIntent.PreWriteAttr),
			Count:           0,
			Committed:       req.Stable,
			Verf:            serverBootTime,
		}, nil
	}

	// A write stores content, and no content store is wired.
	return h.buildWriteErrorResponse(types.StatusForErr(common.ErrNoContentStore), fileHandle, nil, nil), nil
}

// Write Helper Functions

// buildWriteErrorResponse creates a consistent error response with WCC data.
// This centralizes error response creation to reduce duplication.
func (h *Handler) buildWriteErrorResponse(
	status uint32,
	handle metadata.FileHandle,
	preWriteAttr *metadata.FileAttr,
	currentAttr *metadata.FileAttr,
) *WriteResponse {
	wccBefore := xdr.CaptureWccAttr(preWriteAttr)

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
