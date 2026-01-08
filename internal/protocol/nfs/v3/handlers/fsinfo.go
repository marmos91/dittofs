package handlers

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// FsInfoRequest represents an FSINFO request from an NFS client.
// The client sends a file handle (typically the root handle) to query
// filesystem capabilities and limits.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.19 specifies the FSINFO procedure as:
//
//	FSINFO3res NFSPROC3_FSINFO(FSINFO3args) = 19;
//
// Where FSINFO3args contains:
//   - fsroot: File handle for the filesystem root
type FsInfoRequest struct {
	// Handle is the file handle for a filesystem object.
	// Typically this is the root handle obtained from the MOUNT protocol,
	// but can be any valid file handle within the filesystem.
	//
	// The handle is treated as opaque by the protocol layer and validated
	// by the store implementation.
	Handle []byte
}

// FsInfoResponse represents the response to an FSINFO request.
// It contains static information about the NFS server's capabilities,
// preferred transfer sizes, and filesystem properties.
//
// The response is encoded in XDR format before being sent back to the client.
//
// This information helps clients optimize their I/O operations by using
// the server's preferred sizes and understanding what features are supported.
type FsInfoResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains the post-operation attributes of the file system object.
	// Present when Status == types.NFS3OK. May be nil if attributes are unavailable.
	// These attributes help clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Rtmax is the maximum size in bytes of a READ request.
	// Clients should not exceed this value. Only present when Status == types.NFS3OK.
	Rtmax uint32

	// Rtpref is the preferred size in bytes of a READ request.
	// Using this size typically provides optimal performance.
	// Only present when Status == types.NFS3OK.
	Rtpref uint32

	// Rtmult is the suggested multiple for READ request sizes.
	// READ sizes should ideally be multiples of this value for best performance.
	// Only present when Status == types.NFS3OK.
	Rtmult uint32

	// Wtmax is the maximum size in bytes of a WRITE request.
	// Clients should not exceed this value. Only present when Status == types.NFS3OK.
	Wtmax uint32

	// Wtpref is the preferred size in bytes of a WRITE request.
	// Using this size typically provides optimal performance.
	// Only present when Status == types.NFS3OK.
	Wtpref uint32

	// Wtmult is the suggested multiple for WRITE request sizes.
	// WRITE sizes should ideally be multiples of this value for best performance.
	// Only present when Status == types.NFS3OK.
	Wtmult uint32

	// Dtpref is the preferred size in bytes of a READDIR request.
	// Using this size typically provides optimal performance for directory reads.
	// Only present when Status == types.NFS3OK.
	Dtpref uint32

	// Maxfilesize is the maximum file size in bytes supported by the server.
	// Attempts to create or extend files beyond this size will fail.
	// Only present when Status == types.NFS3OK.
	Maxfilesize uint64

	// TimeDelta represents the server's time resolution (granularity).
	// This indicates the smallest time difference the server can reliably distinguish.
	// Only present when Status == types.NFS3OK.
	TimeDelta types.TimeVal

	// Properties is a bitmask of filesystem properties indicating supported features.
	// Only present when Status == types.NFS3OK.
	// Common flags (can be combined with bitwise OR):
	//   - FSFLink (0x0001): Hard links are supported
	//   - FSFSymlink (0x0002): Symbolic links are supported
	//   - FSFHomogeneous (0x0008): PATHCONF is valid for all files
	//   - FSFCanSetTime (0x0010): Server can set file times
	Properties uint32
}

// FsInfo handles the FSINFO procedure, which returns static information about
// the NFS server's capabilities and the filesystem.
//
// The FSINFO procedure provides clients with essential information for optimizing
// their operations:
//  1. Check for context cancellation early
//  2. Validate the file handle format and length
//  3. Verify the file handle exists via store
//  4. Retrieve filesystem capabilities from the store
//  5. Retrieve file attributes for cache consistency
//  6. Return comprehensive filesystem information
//
// Design principles:
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (filesystem limits, capabilities) is delegated to store
//   - File handle validation is performed by store.GetFile()
//   - Context cancellation is checked at strategic points
//   - Comprehensive logging at DEBUG level for troubleshooting
//
// Per RFC 1813 Section 3.3.19:
//
//	"Procedure FSINFO retrieves non-volatile information about a file system.
//	On return, obj_attributes contains the attributes for the file system
//	object specified by fsroot."
//
// Parameters:
//   - ctx: Context information including cancellation, client address and auth flavor
//   - metadataStore: The metadata store containing filesystem configuration
//   - req: The FSINFO request containing the file handle
//
// Returns:
//   - *FsInfoResponse: The response with filesystem information (if successful)
//   - error: Returns error only for internal server failures or context cancellation;
//     protocol-level errors are indicated via the response Status field
//
// RFC 1813 Section 3.3.19: FSINFO Procedure
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &FsInfoRequest{Handle: rootHandle}
//	ctx := &FsInfoContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	}
//	resp, err := handler.FsInfo(ctx, store, req)
//	if err != nil {
//	    // Internal server error or context cancellation occurred
//	    return nil, err
//	}
//	if resp.Status == types.NFS3OK {
//	    // Success - use resp.Rtmax, resp.Wtmax, etc. to optimize I/O
//	}
func (h *Handler) FsInfo(
	ctx *NFSHandlerContext,
	req *FsInfoRequest,
) (*FsInfoResponse, error) {
	logger.DebugCtx(ctx.Context, "FSINFO request", "handle", fmt.Sprintf("0x%x", req.Handle), "client", ctx.ClientAddr, "auth", ctx.AuthFlavor)

	// Check for context cancellation before starting any work
	// FSINFO is lightweight, but we respect cancellation to prevent
	// wasting resources on abandoned requests
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "FSINFO cancelled", "handle", fmt.Sprintf("0x%x", req.Handle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Validate file handle before using it
	if err := validateFileHandle(req.Handle); err != nil {
		logger.DebugCtx(ctx.Context, "FSINFO failed: invalid handle", "error", err)
		return &FsInfoResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadHandle}}, nil
	}

	// ========================================================================
	// Get metadata store from context
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(ctx.Share)
	if err != nil {
		logger.WarnCtx(ctx.Context, "FSINFO failed", "error", err, "handle", fmt.Sprintf("0x%x", req.Handle), "client", xdr.ExtractClientIP(ctx.ClientAddr))
		return &FsInfoResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	// Check for cancellation before store call
	// store operations might involve I/O or locks
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "FSINFO cancelled before GetFile", "handle", fmt.Sprintf("0x%x", req.Handle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Verify the file handle exists and is valid in the store
	// The store is responsible for validating handle format and existence
	file, status, err := h.getFileOrError(ctx, metadataStore, fileHandle, "FSINFO", req.Handle)
	if file == nil {
		return &FsInfoResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	logger.DebugCtx(ctx.Context, "FSINFO", "share", ctx.Share, "path", file.Path)

	// Retrieve filesystem capabilities from the store
	// All business logic about filesystem limits is handled by the store
	// Note: We don't check cancellation here since GetFilesystemCapabilities is typically
	// a fast in-memory operation returning static configuration
	capabilities, err := metadataStore.GetFilesystemCapabilities(ctx.Context, metadata.FileHandle(req.Handle))
	if err != nil {
		traceError(ctx.Context, err, "FSINFO failed to retrieve capabilities", "handle", fmt.Sprintf("0x%x", req.Handle), "client", ctx.ClientAddr)
		return &FsInfoResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Defensive check: ensure store returned valid capabilities
	if capabilities == nil {
		traceError(ctx.Context, fmt.Errorf("store returned nil capabilities"), "FSINFO failed: store returned nil capabilities", "handle", fmt.Sprintf("0x%x", req.Handle), "client", ctx.ClientAddr)
		return &FsInfoResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Generate file ID from handle for attributes
	// This is a protocol-layer concern for creating the NFS attribute structure
	fileid, err := ExtractFileIDFromHandle(req.Handle)
	if err != nil {
		traceError(ctx.Context, err, "FSINFO failed to extract file ID", "handle", fmt.Sprintf("0x%x", req.Handle), "client", ctx.ClientAddr)
		return &FsInfoResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadHandle}}, nil
	}

	// Convert metadata attributes to NFS wire format
	nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

	// Convert timestamp resolution to NFS TimeVal format
	timeDelta := durationToTimeVal(capabilities.TimestampResolution)

	// Build NFS properties bitmask from capabilities
	properties := buildNFSProperties(capabilities)

	logger.InfoCtx(ctx.Context, "FSINFO successful", "client", ctx.ClientAddr, "rtmax", bytesize.ByteSize(capabilities.MaxReadSize), "wtmax", bytesize.ByteSize(capabilities.MaxWriteSize), "maxfilesize", bytesize.ByteSize(capabilities.MaxFileSize), "properties", fmt.Sprintf("0x%x", properties))

	// Build response with data from store
	// Map the standardized FilesystemCapabilities to NFS wire format
	return &FsInfoResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
		Rtmax:           capabilities.MaxReadSize,
		Rtpref:          capabilities.PreferredReadSize,
		Rtmult:          4096, // Common block size multiple
		Wtmax:           capabilities.MaxWriteSize,
		Wtpref:          capabilities.PreferredWriteSize,
		Wtmult:          4096, // Common block size multiple
		Dtpref:          8192, // Reasonable default for directory reads
		Maxfilesize:     capabilities.MaxFileSize,
		TimeDelta:       timeDelta,
		Properties:      properties,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// buildNFSProperties builds the NFS properties bitmask from FilesystemCapabilities.
//
// This translates the generic capability flags into NFS-specific property bits.
func buildNFSProperties(cap *metadata.FilesystemCapabilities) uint32 {
	var properties uint32

	if cap.SupportsHardLinks {
		properties |= types.FSFLink
	}
	if cap.SupportsSymlinks {
		properties |= types.FSFSymlink
	}

	// Always set FSFHomogeneous - PATHCONF is valid for all files
	properties |= types.FSFHomogeneous

	// Always set FSFCanSetTime - server can set file times via SETATTR
	properties |= types.FSFCanSetTime

	return properties
}

// durationToTimeVal converts a time.Duration to NFS TimeVal format.
//
// The TimeVal represents the server's time resolution - the smallest
// time difference it can reliably distinguish.
func durationToTimeVal(d time.Duration) types.TimeVal {
	seconds := uint32(d / time.Second)
	nanoseconds := uint32(d % time.Second)

	return types.TimeVal{
		Seconds:  seconds,
		Nseconds: nanoseconds,
	}
}

// ============================================================================
// Utility Functions
// ============================================================================

// validateFileHandle performs basic validation on a file handle.
// This includes checking for nil, empty, and excessively long handles.
//
// Returns nil if the handle is valid, error otherwise.
func validateFileHandle(handle []byte) error {
	if handle == nil {
		return fmt.Errorf("handle is nil")
	}

	if len(handle) == 0 {
		return fmt.Errorf("handle is empty")
	}

	// NFS v3 handles should not exceed 64 bytes per RFC 1813
	if len(handle) > 64 {
		return fmt.Errorf("handle too long: %d bytes (max 64)", len(handle))
	}

	// Handle must be at least 8 bytes to extract a file ID
	// This is a protocol-specific requirement for the file ID extraction
	if len(handle) < 8 {
		return fmt.Errorf("handle too short: %d bytes (min 8 for file ID)", len(handle))
	}

	return nil
}

// ExtractFileIDFromHandle extracts a file ID from a file handle.
//
// This is a thin wrapper around metadata.HandleToINode() which provides the
// canonical implementation for converting file handles to inode numbers.
//
// IMPORTANT: All code that needs to convert handles to file IDs MUST use
// metadata.HandleToINode() to ensure consistent inode generation across
// the system. Using different methods will cause "fileid changed" errors
// from NFS clients.
//
// Parameters:
//   - handle: The file handle
//
// Returns:
//   - uint64: The extracted file ID (SHA-256 hash of handle)
//   - error: Always nil (kept for API compatibility)
func ExtractFileIDFromHandle(handle []byte) (uint64, error) {
	return metadata.HandleToINode(handle), nil
}
