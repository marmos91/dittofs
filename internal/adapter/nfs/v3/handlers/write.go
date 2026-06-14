package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.DebugCtx(ctx.Context, "WRITE", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", bytesize.ByteSize(req.Offset), "count", bytesize.ByteSize(req.Count), "stable", req.Stable, "client", clientIP, "auth", ctx.AuthFlavor)

	if ctx.isContextCancelled() {
		logWarn(ctx.Context, ctx.Context.Err(), "WRITE cancelled", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Structural validation only (handle, overflow, stability). The size cap
	// is applied as a short-write below, derived from the advertised wtmax.

	if err := validateWriteRequest(req); err != nil {
		logWarn(ctx.Context, err, "WRITE validation failed", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)
		return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	metaSvc, blockStore, err := getServicesForHandle(h.Registry, ctx.Context, fileHandle)
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
	maxWriteSize := cachedMaxWriteSize()
	if maxWriteSize == 0 {
		if caps, capErr := metaSvc.GetFilesystemCapabilities(ctx.Context, fileHandle); capErr == nil && caps != nil {
			maxWriteSize = caps.MaxWriteSize
			setMaxWriteSize(maxWriteSize)
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
			logger.DebugCtx(ctx.Context, "WRITE cancelled during auth context building", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP, "error", ctx.Context.Err())
			return &WriteResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		logError(ctx.Context, err, "WRITE failed: failed to build auth context", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)

		// No WCC data available - we haven't called PrepareWrite yet
		return &WriteResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
		}, nil
	}

	// Fire-and-forget: per Samba behavior, NFS proceeds even if break is pending.
	// The break notification is sent to the SMB client asynchronously.
	if breaker := h.getOplockBreaker(); breaker != nil {
		if err := breaker.CheckAndBreakForWrite(ctx.Context, lock.FileHandle(string(fileHandle))); err != nil {
			logger.Debug("NFS WRITE: oplock break initiated",
				"handle", fileHandle, "result", err)
		}
	}

	// PrepareWrite validates permissions but does NOT modify metadata yet.
	// Metadata is updated by CommitWrite after BlockStore write succeeds.

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
		status := common.MapToNFS3(err)

		logger.WarnCtx(ctx.Context, "WRITE failed: PrepareWrite error", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", len(req.Data), "client", clientIP, "error", err)

		// No WCC data available - PrepareWrite failed so we don't have file attributes
		return h.buildWriteErrorResponse(status, fileHandle, nil, nil), nil
	}

	// Build WCC attributes from pre-write state
	nfsWccAttr := xdr.CaptureWccAttr(writeIntent.PreWriteAttr)

	// Check context before write operation
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "WRITE cancelled before write", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", req.Count, "client", clientIP, "error", ctx.Context.Err())
		return h.buildWriteErrorResponse(types.NFS3ErrIO, fileHandle, writeIntent.PreWriteAttr, writeIntent.PreWriteAttr), nil
	}

	// Write to BlockStore (uses local cache, will be flushed on COMMIT).
	// Routed through common.WriteToBlockStore so any future []BlockRef
	// plumbing lands in one place (see common/doc.go).
	err = common.WriteToBlockStore(ctx.Context, blockStore, writeIntent.PayloadID, req.Data, req.Offset)
	if err != nil {
		logError(ctx.Context, err, "WRITE failed: BlockStore write error", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", len(req.Data), "payload_id", writeIntent.PayloadID, "client", clientIP)
		status := common.MapContentToNFS3(err)
		return h.buildWriteErrorResponse(status, fileHandle, writeIntent.PreWriteAttr, writeIntent.PreWriteAttr), nil
	}
	logger.DebugCtx(ctx.Context, "WRITE: cached successfully", "payload_id", writeIntent.PayloadID)

	updatedFile, err := metaSvc.CommitWrite(authCtx, writeIntent)
	if err != nil {
		logError(ctx.Context, err, "WRITE failed: CommitWrite error (content written but metadata not updated)", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "count", len(req.Data), "client", clientIP)

		// Content is written but metadata not updated - this is an inconsistent state
		// Map error to NFS status
		status := common.MapToNFS3(err)

		return h.buildWriteErrorResponse(status, fileHandle, writeIntent.PreWriteAttr, writeIntent.PreWriteAttr), nil
	}

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &updatedFile.FileAttr)

	logger.DebugCtx(ctx.Context, "WRITE successful", "file", updatedFile.PayloadID, "offset", bytesize.ByteSize(req.Offset), "requested", bytesize.ByteSize(req.Count), "written", bytesize.ByteSize(len(req.Data)), "new_size", bytesize.ByteSize(updatedFile.Size), "client", clientIP)

	// Stability Level (RFC 1813 Section 3.3.7)
	//
	// The `stable` field is the client's request. The server reports what it
	// ACTUALLY did via the `committed` field.
	//
	// Default: UNSTABLE. Cache is always enabled, so an unstable WRITE leaves the
	// data in the local cache (crash-safe via the WAL) and the client calls COMMIT
	// when it needs durability. This is much faster for high-latency backends (S3
	// writes are 100ms+; batching on COMMIT is 10-100x faster for sequential I/O).
	//
	// When the client requests DATA_SYNC or FILE_SYNC, RFC 1813 requires the data
	// (and, for FILE_SYNC, metadata) to be on stable storage before the reply. We
	// honor that by flushing this file synchronously and reporting the requested
	// stability level, mirroring the COMMIT path. On flush failure we fall back to
	// reporting UNSTABLE rather than failing the WRITE — the bytes are still in the
	// crash-safe cache and the client can retry COMMIT.
	committed := uint32(UnstableWrite)
	if req.Stable >= DataSyncWrite {
		if err := h.flushStableWrite(ctx, metaSvc, blockStore, fileHandle, writeIntent.PayloadID, authCtx, req.Stable); err != nil {
			logError(ctx.Context, err, "WRITE: stable flush failed, downgrading to UNSTABLE",
				"handle", fmt.Sprintf("0x%x", req.Handle), "stable_requested", req.Stable, "client", clientIP)
		} else {
			committed = req.Stable
		}
	}

	logger.DebugCtx(ctx.Context, "WRITE details", "stable_requested", req.Stable, "committed", committed, "size", bytesize.ByteSize(updatedFile.Size), "type", updatedFile.Type, "mode", fmt.Sprintf("%o", updatedFile.Mode))

	return &WriteResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		AttrBefore:      nfsWccAttr,
		AttrAfter:       nfsAttr,
		// Count: RFC 1813 specifies this as "The number of bytes of data written".
		// We currently assume that the block store WriteAt is all-or-nothing:
		// it either writes all bytes or fails entirely. Under that assumption,
		// len(req.Data) equals the actual bytes written on success.
		// NOTE: If a future block store allows partial writes, this code must
		// be updated to report the actual number of bytes written instead of
		// len(req.Data), and the WriteAt contract should document that behavior.
		Count:     uint32(len(req.Data)),
		Committed: committed,      // requested stability if flushed, else UNSTABLE (client calls COMMIT)
		Verf:      serverBootTime, // Server boot time for restart detection
	}, nil
}

// Write Helper Functions

// flushStableWrite forces this file's cached data (and, for FILE_SYNC, metadata)
// to stable storage so a DATA_SYNC / FILE_SYNC WRITE can be acknowledged as
// committed, per RFC 1813 Section 3.3.7. It mirrors the COMMIT path: flush the
// block store, then persist any pending metadata for the file.
//
// RFC 1813 distinguishes the two stable levels:
//   - DATA_SYNC (1): only the file data must be on stable storage. A metadata
//     flush failure is tolerated — it is reconciled by a later COMMIT — and the
//     write is still reported at the requested level.
//   - FILE_SYNC (2): both data AND metadata must be on stable storage before the
//     reply. A metadata flush failure must therefore propagate so the caller
//     reports UNSTABLE instead of falsely claiming FILE_SYNC durability.
func (h *Handler) flushStableWrite(
	ctx *NFSHandlerContext,
	metaSvc *metadata.Service,
	blockStore *engine.Store,
	handle metadata.FileHandle,
	payloadID metadata.PayloadID,
	authCtx *metadata.AuthContext,
	stable uint32,
) error {
	if err := common.CommitBlockStore(ctx.Context, blockStore, payloadID); err != nil {
		return err
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, handle); err != nil {
		if stable >= FileSyncWrite {
			// FILE_SYNC requires durable metadata; surface the failure.
			return err
		}
		logger.WarnCtx(ctx.Context, "WRITE: DATA_SYNC metadata flush failed (data durable, will reconcile)",
			"handle", fmt.Sprintf("0x%x", handle), "error", err)
	}
	return nil
}

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
