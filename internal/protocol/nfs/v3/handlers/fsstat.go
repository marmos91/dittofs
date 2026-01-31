package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// FsStatRequest represents an FSSTAT request from an NFS client.
// The client provides a file handle to query filesystem statistics for.
//
// RFC 1813 Section 3.3.18 specifies the FSSTAT procedure as:
//
//	FSSTAT3res NFSPROC3_FSSTAT(FSSTAT3args) = 18;
//
// The request contains only a file handle, typically the root handle of the
// mounted filesystem.
type FsStatRequest struct {
	// Handle is the file handle for which to retrieve filesystem statistics.
	// This is typically the root handle obtained from the MOUNT procedure.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// FsStatResponse represents the response to an FSSTAT request.
// It contains the status of the operation and, if successful, the current
// filesystem statistics and optional post-operation attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type FsStatResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains the post-operation attributes for the file handle.
	// This is optional and may be nil if Status != types.NFS3OK.
	// Including attributes helps clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Tbytes is the total size of the filesystem in bytes.
	// Only present when Status == types.NFS3OK.
	Tbytes uint64

	// Fbytes is the free space available in bytes.
	// Only present when Status == types.NFS3OK.
	Fbytes uint64

	// Abytes is the free space available to non-privileged users in bytes.
	// This may be less than Fbytes if space is reserved for root/admin.
	// Only present when Status == types.NFS3OK.
	Abytes uint64

	// Tfiles is the total number of file slots (inodes) in the filesystem.
	// Only present when Status == types.NFS3OK.
	Tfiles uint64

	// Ffiles is the number of free file slots available.
	// Only present when Status == types.NFS3OK.
	Ffiles uint64

	// Afiles is the number of file slots available to non-privileged users.
	// This may be less than Ffiles if slots are reserved for root/admin.
	// Only present when Status == types.NFS3OK.
	Afiles uint64

	// Invarsec is the number of seconds for which the filesystem is not
	// expected to change. A value of 0 means the filesystem is expected to
	// change at any time. This helps clients optimize stat operations.
	// Only present when Status == types.NFS3OK.
	Invarsec uint32
}

// ============================================================================
// Handler Implementation
// ============================================================================

// FsStat handles the FSSTAT procedure, which returns dynamic information about
// the filesystem's current state, including space usage and available inodes.
//
// The FSSTAT process follows these steps:
//  1. Check for context cancellation before starting
//  2. Validate the file handle format and size
//  3. Verify the file handle exists via store.GetFile()
//  4. Retrieve current filesystem statistics from the store
//  5. Retrieve file attributes for cache consistency
//  6. Return comprehensive filesystem statistics to the client
//
// Design principles:
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (space calculations, limits) is delegated to store
//   - File handle validation is performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//   - Respects context cancellation for graceful shutdown and timeouts
//
// Security considerations:
//   - Handle validation prevents malformed requests from causing errors
//   - store layer enforces access control if needed
//   - Client context enables auditing and rate limiting
//   - No sensitive information leaked in error messages
//
// Context cancellation:
//   - Checks at operation start before any work
//   - Checks before each store call (GetFile, GetFilesystemStatistics)
//   - Returns types.NFS3ErrIO on cancellation
//   - Useful for client disconnects, server shutdown, request timeouts
//
// Per RFC 1813 Section 3.3.18:
//
//	"Procedure FSSTAT retrieves volatile information about a file system.
//	 The semantics of the size and space related fields are:
//	 - tbytes: Total size of the file system in bytes
//	 - fbytes: Free bytes in the file system
//	 - abytes: Number of free bytes available to non-privileged users"
//
// Parameters:
//   - ctx: Context information including cancellation, client address and auth flavor
//   - metadataStore: The metadata store containing filesystem statistics
//   - req: The FSSTAT request containing the file handle
//
// Returns:
//   - *FsStatResponse: The response with filesystem statistics (if successful)
//   - error: Returns error only for internal server failures; protocol-level
//     errors are indicated via the response Status field
//
// RFC 1813 Section 3.3.18: FSSTAT Procedure
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &FsStatRequest{Handle: rootHandle}
//	ctx := &FsStatContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	}
//	resp, err := handler.FsStat(ctx, store, req)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Use resp.Tbytes, resp.Fbytes, etc. for filesystem stats
//	}
func (h *Handler) FsStat(
	ctx *NFSHandlerContext,
	req *FsStatRequest,
) (*FsStatResponse, error) {
	logger.DebugCtx(ctx.Context, "FSSTAT request", "handle", fmt.Sprintf("%x", req.Handle), "client", ctx.ClientAddr)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "FSSTAT cancelled", "handle", fmt.Sprintf("%x", req.Handle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Validate file handle
	// ========================================================================

	// Validate file handle
	if len(req.Handle) == 0 {
		logger.WarnCtx(ctx.Context, "FSSTAT failed: empty file handle", "client", ctx.ClientAddr)
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadHandle}}, nil
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		logger.WarnCtx(ctx.Context, "FSSTAT failed: oversized handle", "bytes", len(req.Handle), "client", ctx.ClientAddr)
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadHandle}}, nil
	}

	// Validate handle length for file ID extraction (need at least 8 bytes)
	if len(req.Handle) < 8 {
		logger.WarnCtx(ctx.Context, "FSSTAT failed: undersized handle", "bytes", len(req.Handle), "client", ctx.ClientAddr)
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadHandle}}, nil
	}

	// ========================================================================
	// Step 3: Verify the file handle exists
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "FSSTAT cancelled before GetFile", "handle", fmt.Sprintf("%x", req.Handle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Get metadata from registry
	// ========================================================================

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "FSSTAT failed: metadata service not initialized", "client", ctx.ClientAddr, "error", err)
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	file, status, err := h.getFileOrError(ctx, fileHandle, "FSSTAT", req.Handle)
	if file == nil {
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	logger.DebugCtx(ctx.Context, "FSSTAT", "share", ctx.Share, "path", file.Path)

	// ========================================================================
	// Step 4: Retrieve filesystem statistics from the store
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "FSSTAT cancelled before GetFilesystemStatistics", "handle", fmt.Sprintf("%x", req.Handle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	stats, err := metaSvc.GetFilesystemStatistics(ctx.Context, metadata.FileHandle(req.Handle))
	if err != nil {
		traceError(ctx.Context, err, "FSSTAT failed: error retrieving statistics", "client", ctx.ClientAddr)
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Defensive check: ensure store returned valid statistics
	if stats == nil {
		traceError(ctx.Context, fmt.Errorf("store returned nil statistics"), "FSSTAT failed: store returned nil statistics", "client", ctx.ClientAddr)
		return &FsStatResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 5: Build success response with file attributes and stats
	// ========================================================================

	// Generate file ID from handle for attributes
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

	// Convert ValidFor duration to seconds for Invarsec
	invarsec := uint32(stats.ValidFor.Seconds())

	logger.InfoCtx(ctx.Context, "FSSTAT successful", "client", ctx.ClientAddr, "total", bytesize.ByteSize(stats.TotalBytes), "used", bytesize.ByteSize(stats.UsedBytes), "avail", bytesize.ByteSize(stats.AvailableBytes), "tfiles", stats.TotalFiles, "ffiles", stats.UsedFiles, "afiles", stats.AvailableFiles)

	// Build response with data from store
	// Note: NFS uses "free bytes" while the interface tracks "used bytes"
	// Calculate free bytes as: total - used
	freeBytes := stats.TotalBytes - stats.UsedBytes

	return &FsStatResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
		Tbytes:          stats.TotalBytes,
		Fbytes:          freeBytes,
		Abytes:          stats.AvailableBytes,
		Tfiles:          stats.TotalFiles,
		Ffiles:          stats.TotalFiles - stats.UsedFiles, // Free = Total - Used
		Afiles:          stats.AvailableFiles,
		Invarsec:        invarsec,
	}, nil
}
