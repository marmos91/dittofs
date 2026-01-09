package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// GetAttrRequest represents a GETATTR request from an NFS client.
// The client provides a file handle to retrieve attributes for.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.1 specifies the GETATTR procedure as:
//
//	GETATTR3res NFSPROC3_GETATTR(GETATTR3args) = 1;
//
// The GETATTR procedure is used to obtain attributes for a file system object.
// This is one of the most frequently called NFS procedures.
type GetAttrRequest struct {
	// Handle is the file handle of the object to get attributes for.
	// Must be a valid file handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// GetAttrResponse represents the response to a GETATTR request.
// It contains the status of the operation and, if successful, the file attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type GetAttrResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains the file attributes.
	// Only present when Status == types.NFS3OK.
	// Includes file type, permissions, ownership, size, timestamps, etc.
	Attr *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// GetAttr returns the attributes for a file system object.
//
// This implements the NFS GETATTR procedure as defined in RFC 1813 Section 3.3.1.
//
// **Purpose:**
//
// GETATTR is the fundamental operation for retrieving file metadata. It's used by:
//   - Clients to check if cached attributes are still valid
//   - The 'stat' and 'ls' commands to display file information
//   - The NFS client to validate file handles before operations
//   - Cache consistency protocols to detect file changes
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format and length)
//  3. Verify file handle exists via store.GetFile()
//  4. Generate file attributes with proper file ID
//  5. Return attributes to client
//
// **Context cancellation:**
//
//   - Single check at the beginning to respect client disconnection
//   - Check after GetFile to catch cancellation during lookup
//   - Minimal overhead to maintain high performance for this frequent operation
//   - Returns NFS3ErrIO status with context error for cancellation
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (file lookup, attribute generation) is delegated to store
//   - File handle validation is performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Performance Considerations:**
//
// GETATTR is one of the most frequently called NFS procedures. Implementations should:
//   - Cache attributes when possible
//   - Minimize store access overhead
//   - Use efficient file ID generation
//   - Avoid unnecessary data copying
//   - Minimize context cancellation checks (only 2 checks for performance)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Stale handle → NFS3ErrStale
//   - I/O error → types.NFS3ErrIO
//   - Invalid handle → types.NFS3ErrBadHandle
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store layer can enforce access control if needed
//   - Client context enables audit logging
//   - No sensitive information leaked in error messages
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication flavor
//   - metadataStore: The metadata store for file access
//   - req: The getattr request containing the file handle
//
// **Returns:**
//   - *GetAttrResponse: Response with status and attributes (if successful)
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.1: GETATTR Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &GetAttrRequest{Handle: fileHandle}
//	ctx := &GetAttrContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 0, // AUTH_NULL
//	}
//	resp, err := handler.GetAttr(ctx, store, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    // Use resp.Attr for file information
//	}
func (h *Handler) GetAttr(
	ctx *NFSHandlerContext,
	req *GetAttrRequest,
) (*GetAttrResponse, error) {
	// Check for cancellation before starting any work
	// This is the only pre-operation check for GETATTR to minimize overhead
	// GETATTR is one of the most frequently called procedures, so we optimize
	// for the common case of no cancellation
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "GETATTR cancelled before processing",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", ctx.ClientAddr,
			"error", ctx.Context.Err())
		return &GetAttrResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "GETATTR",
		"handle", fmt.Sprintf("%x", req.Handle),
		"client", clientIP,
		"auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateGetAttrRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "GETATTR validation failed",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP,
			"error", err)
		return &GetAttrResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata from registry
	// ========================================================================

	_ = h.Registry.GetMetadataService() // metaSvc available if needed later

	// ========================================================================
	// Step 3: Verify file handle exists and retrieve attributes
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)

	file, status, err := h.getFileOrError(ctx, fileHandle, "GETATTR", req.Handle)
	if file == nil {
		return &GetAttrResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	logger.DebugCtx(ctx.Context, "GETATTR",
		"share", ctx.Share,
		"path", file.Path)

	// ========================================================================
	// Step 4: Generate file attributes with proper file ID
	// ========================================================================
	// The file ID is extracted from the handle for NFS protocol purposes.
	// This is a protocol-layer concern for creating the wire format.
	// No cancellation check here - this operation is extremely fast (pure computation)

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

	logger.InfoCtx(ctx.Context, "GETATTR successful",
		"handle", fmt.Sprintf("%x", req.Handle),
		"type", nfsAttr.Type,
		"mode", fmt.Sprintf("%o", nfsAttr.Mode),
		"size", nfsAttr.Size,
		"nlink", nfsAttr.Nlink,
		"client", clientIP)

	logger.DebugCtx(ctx.Context, "GETATTR details",
		"handle", fmt.Sprintf("%x", fileHandle),
		"uid", nfsAttr.UID,
		"gid", nfsAttr.GID,
		"mtime", fmt.Sprintf("%d.%d", nfsAttr.Mtime.Seconds, nfsAttr.Mtime.Nseconds))

	return &GetAttrResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// getAttrValidationError represents a GETATTR request validation error.
type getAttrValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *getAttrValidationError) Error() string {
	return e.message
}

// validateGetAttrRequest validates GETATTR request parameters.
//
// Checks performed:
//   - File handle is not nil or empty
//   - File handle length is within RFC 1813 limits (max 64 bytes)
//   - File handle is long enough for file ID extraction (min 8 bytes)
//
// Returns:
//   - nil if valid
//   - *getAttrValidationError with NFS status if invalid
func validateGetAttrRequest(req *GetAttrRequest) *getAttrValidationError {
	// Validate file handle presence
	if len(req.Handle) == 0 {
		return &getAttrValidationError{
			message:   "file handle is empty",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &getAttrValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	// This is a protocol-specific requirement for generating the fileid field
	if len(req.Handle) < 8 {
		return &getAttrValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	return nil
}
