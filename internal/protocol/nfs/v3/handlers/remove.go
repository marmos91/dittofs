package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// RemoveRequest represents a REMOVE request from an NFS client.
// The client provides a directory handle and filename to delete.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.12 specifies the REMOVE procedure as:
//
//	REMOVE3res NFSPROC3_REMOVE(REMOVE3args) = 12;
//
// The REMOVE procedure deletes a file from a directory. It cannot be used
// to remove directories (use RMDIR for that). The operation is atomic from
// the client's perspective.
type RemoveRequest struct {
	// DirHandle is the file handle of the parent directory containing the file.
	// Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Filename is the name of the file to remove from the directory.
	// Must follow NFS naming conventions:
	//   - Cannot be empty, ".", or ".."
	//   - Maximum length is 255 bytes per NFS specification
	//   - Should not contain null bytes or path separators (/)
	Filename string
}

// RemoveResponse represents the response to a REMOVE request.
// It contains the status of the operation and WCC (Weak Cache Consistency)
// data for the parent directory.
//
// The response is encoded in XDR format before being sent back to the client.
type RemoveResponse struct {
	// Status indicates the result of the remove operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): File or directory not found
	//   - types.NFS3ErrNotDir (20): DirHandle is not a directory
	//   - types.NFS3ErrIsDir (21): Attempted to remove a directory (use RMDIR)
	//   - NFS3ErrAcces (13): Permission denied
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	//   - NFS3ErrNameTooLong (63): Filename too long
	Status uint32

	// DirWccBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency to help clients detect changes.
	// May be nil if attributes could not be captured.
	DirWccBefore *types.WccAttr

	// DirWccAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency to provide updated directory state.
	// May be nil on error, but should be present on success.
	DirWccAfter *types.NFSFileAttr
}

// RemoveContext contains the context information needed to process a REMOVE request.
// This includes client identification, authentication details, and cancellation handling
// for access control.
type RemoveContext struct {
	// Context carries cancellation signals and deadlines
	// The Remove handler checks this context to abort operations if the client
	// disconnects or the request times out
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	AuthFlavor uint32

	// UID is the authenticated user ID (from AUTH_UNIX).
	// Used for access control checks by the store.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for access control checks by the store.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for checking if user belongs to file's group.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// Implement NFSAuthContext interface for RemoveContext
func (c *RemoveContext) GetContext() context.Context { return c.Context }
func (c *RemoveContext) GetClientAddr() string       { return c.ClientAddr }
func (c *RemoveContext) GetAuthFlavor() uint32       { return c.AuthFlavor }
func (c *RemoveContext) GetUID() *uint32             { return c.UID }
func (c *RemoveContext) GetGID() *uint32             { return c.GID }
func (c *RemoveContext) GetGIDs() []uint32           { return c.GIDs }


// ============================================================================
// Protocol Handler
// ============================================================================

// Remove deletes a file from a directory.
//
// This implements the NFS REMOVE procedure as defined in RFC 1813 Section 3.3.12.
//
// **Purpose:**
//
// REMOVE deletes a regular file (not a directory) from a parent directory.
// It is one of the fundamental file system operations. Common use cases:
//   - Deleting temporary files
//   - Removing old or unused files
//   - Cleaning up workspace
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format, filename syntax)
//  3. Extract client IP and authentication credentials from context
//  4. Verify parent directory exists (via store)
//  5. Capture pre-operation directory state (for WCC)
//  6. Check for cancellation before remove operation
//  7. Delegate file removal to store.RemoveFile()
//  8. Return updated directory WCC data
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after directory lookup (before atomic remove operation)
//   - No check during RemoveFile to maintain atomicity
//   - Returns NFS3ErrIO status with context error for cancellation
//   - Always includes WCC data for cache consistency
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (deletion, validation, access control) delegated to store
//   - File handle validation performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the store, which can implement:
//   - Write permission checking on the parent directory
//   - Access control based on UID/GID
//   - Ownership verification (can only delete own files, or root can delete any)
//
// **REMOVE vs RMDIR:**
//
// REMOVE is for files only:
//   - Regular files: Success
//   - Directories: Returns types.NFS3ErrIsDir (must use RMDIR)
//   - Symbolic links: Success (removes the link, not the target)
//   - Special files: Success (device files, sockets, FIFOs)
//
// **Atomicity:**
//
// From the client's perspective, REMOVE is atomic. Either:
//   - The file is completely removed (success)
//   - The file remains unchanged (failure)
//
// There should be no intermediate state visible to clients.
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes:
//   - Directory not found → types.NFS3ErrNoEnt
//   - Not a directory → types.NFS3ErrNotDir
//   - File not found → types.NFS3ErrNoEnt
//   - File is directory → types.NFS3ErrIsDir
//   - Permission denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients maintain cache coherency:
//  1. Capture directory attributes before the operation (WccBefore)
//  2. Perform the file removal
//  3. Capture directory attributes after the operation (WccAfter)
//
// Clients use this to:
//   - Detect if directory changed during the operation
//   - Update their cached directory attributes
//   - Invalidate stale cached data
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store enforces write permission on parent directory
//   - Filename validation prevents directory traversal attacks
//   - Client context enables audit logging
//   - Cannot delete directories (prevents accidental data loss)
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - metadataStore: The metadata store for file and directory operations
//   - req: The remove request containing directory handle and filename
//
// **Returns:**
//   - *RemoveResponse: Response with status and directory WCC data
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.12: REMOVE Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &RemoveRequest{
//	    DirHandle: dirHandle,
//	    Filename:  "oldfile.txt",
//	}
//	ctx := &RemoveContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Remove(ctx, store, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    // File removed successfully
//	}
func (h *DefaultNFSHandler) Remove(
	ctx *RemoveContext,
	contentStore content.ContentStore,
	metadataStore metadata.MetadataStore,
	req *RemoveRequest,
) (*RemoveResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	select {
	case <-ctx.Context.Done():
		logger.Debug("REMOVE cancelled before processing: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, ctx.ClientAddr, ctx.Context.Err())
		return &RemoveResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("REMOVE: file='%s' dir=%x client=%s auth=%d",
		req.Filename, req.DirHandle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateRemoveRequest(req); err != nil {
		logger.Warn("REMOVE validation failed: file='%s' client=%s error=%v",
			req.Filename, clientIP, err)
		return &RemoveResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Capture pre-operation directory attributes for WCC
	// ========================================================================

	dirHandle := metadata.FileHandle(req.DirHandle)
	dirAttr, err := metadataStore.GetFile(ctx.Context, dirHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("REMOVE cancelled during directory lookup: file='%s' dir=%x client=%s error=%v",
				req.Filename, req.DirHandle, clientIP, ctx.Context.Err())
			return &RemoveResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Warn("REMOVE failed: directory not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &RemoveResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(dirAttr)

	// Check for cancellation before the remove operation
	// This is the most critical check - we don't want to start removing
	// the file if the client has already disconnected
	select {
	case <-ctx.Context.Done():
		logger.Debug("REMOVE cancelled before remove operation: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, clientIP, ctx.Context.Err())

		dirID := xdr.ExtractFileID(dirHandle)
		wccAfter := xdr.MetadataToNFS(dirAttr, dirID)

		return &RemoveResponse{
			Status:       types.NFS3ErrIO,
			DirWccBefore: wccBefore,
			DirWccAfter:  wccAfter,
		}, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 3: Build authentication context with share-level identity mapping
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, metadataStore, dirHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("REMOVE cancelled during auth context building: file='%s' dir=%x client=%s error=%v",
				req.Filename, req.DirHandle, clientIP, ctx.Context.Err())

			dirID := xdr.ExtractFileID(dirHandle)
			wccAfter := xdr.MetadataToNFS(dirAttr, dirID)

			return &RemoveResponse{
				Status:       types.NFS3ErrIO,
				DirWccBefore: wccBefore,
				DirWccAfter:  wccAfter,
			}, ctx.Context.Err()
		}

		logger.Error("REMOVE failed: failed to build auth context: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, clientIP, err)

		dirID := xdr.ExtractFileID(dirHandle)
		wccAfter := xdr.MetadataToNFS(dirAttr, dirID)

		return &RemoveResponse{
			Status:       types.NFS3ErrIO,
			DirWccBefore: wccBefore,
			DirWccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4: Remove file via store
	// ========================================================================
	// The store handles:
	// - Verifying parent is a directory
	// - Verifying the file exists
	// - Checking it's not a directory (must use RMDIR for directories)
	// - Verifying write permission on the parent directory
	// - Removing the file from the directory
	// - Deleting the file metadata
	// - Updating parent directory timestamps
	//
	// We don't check for cancellation inside RemoveFile to maintain atomicity.
	// The store should respect context internally for its operations.

	removedFileAttr, err := metadataStore.RemoveFile(authCtx, dirHandle, req.Filename)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("REMOVE cancelled during remove operation: file='%s' dir=%x client=%s error=%v",
				req.Filename, req.DirHandle, clientIP, ctx.Context.Err())

			// Get updated directory attributes for WCC data (best effort)
			var wccAfter *types.NFSFileAttr
			if dirAttr, getErr := metadataStore.GetFile(ctx.Context, dirHandle); getErr == nil {
				dirID := xdr.ExtractFileID(dirHandle)
				wccAfter = xdr.MetadataToNFS(dirAttr, dirID)
			}

			return &RemoveResponse{
				Status:       types.NFS3ErrIO,
				DirWccBefore: wccBefore,
				DirWccAfter:  wccAfter,
			}, ctx.Context.Err()
		}

		// Map store errors to NFS status codes
		nfsStatus := xdr.MapStoreErrorToNFSStatus(err, clientIP, "REMOVE")

		// Get updated directory attributes for WCC data (best effort)
		var wccAfter *types.NFSFileAttr
		if dirAttr, getErr := metadataStore.GetFile(ctx.Context, dirHandle); getErr == nil {
			dirID := xdr.ExtractFileID(dirHandle)
			wccAfter = xdr.MetadataToNFS(dirAttr, dirID)
		}

		return &RemoveResponse{
			Status:       nfsStatus,
			DirWccBefore: wccBefore,
			DirWccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4.5: Delete content if file has content and store supports deletion
	// ========================================================================
	// After successfully removing the metadata, attempt to delete the actual
	// file content. This is done after metadata removal to ensure consistency:
	// if metadata is removed but content deletion fails, the content becomes
	// orphaned but the file is still properly deleted from the client's view.

	if removedFileAttr.ContentID != "" {
		if writableStore, ok := contentStore.(content.WritableContentStore); ok {
			if err := writableStore.Delete(ctx.Context, removedFileAttr.ContentID); err != nil {
				// Log but don't fail the operation - metadata is already removed
				logger.Warn("REMOVE: failed to delete content for file='%s' content_id=%s: %v",
					req.Filename, removedFileAttr.ContentID, err)
				// This is non-fatal - the file is successfully removed from metadata
				// The orphaned content can be cleaned up later via garbage collection
			} else {
				logger.Debug("REMOVE: deleted content for file='%s' content_id=%s",
					req.Filename, removedFileAttr.ContentID)
			}
		} else {
			logger.Debug("REMOVE: content store does not support deletion (read-only)")
		}
	}

	// ========================================================================
	// Step 5: Build success response with updated directory attributes
	// ========================================================================

	// Get updated directory attributes for WCC data
	dirAttr, err = metadataStore.GetFile(ctx.Context, dirHandle)
	if err != nil {
		logger.Warn("REMOVE: file removed but cannot get updated directory attributes: dir=%x error=%v",
			req.DirHandle, err)
		// Continue with nil WccAfter rather than failing the entire operation
	}

	var wccAfter *types.NFSFileAttr
	if dirAttr != nil {
		dirID := xdr.ExtractFileID(dirHandle)
		wccAfter = xdr.MetadataToNFS(dirAttr, dirID)
	}

	logger.Info("REMOVE successful: file='%s' dir=%x client=%s",
		req.Filename, req.DirHandle, clientIP)

	// Convert internal type to NFS type for logging
	nfsType := uint32(removedFileAttr.Type) + 1 // Internal types are 0-based, NFS types are 1-based
	logger.Debug("REMOVE details: file_type=%d file_size=%d",
		nfsType, removedFileAttr.Size)

	return &RemoveResponse{
		Status:       types.NFS3OK,
		DirWccBefore: wccBefore,
		DirWccAfter:  wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// removeValidationError represents a REMOVE request validation error.
type removeValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *removeValidationError) Error() string {
	return e.message
}

// validateRemoveRequest validates REMOVE request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Filename is valid (not empty, not "." or "..", length, characters)
//
// Returns:
//   - nil if valid
//   - *removeValidationError with NFS status if invalid
func validateRemoveRequest(req *RemoveRequest) *removeValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &removeValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &removeValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &removeValidationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate filename
	if req.Filename == "" {
		return &removeValidationError{
			message:   "empty filename",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Filename == "." || req.Filename == ".." {
		return &removeValidationError{
			message:   fmt.Sprintf("cannot remove '%s'", req.Filename),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check filename length (NFS limit is typically 255 bytes)
	if len(req.Filename) > 255 {
		return &removeValidationError{
			message:   fmt.Sprintf("filename too long: %d bytes (max 255)", len(req.Filename)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if strings.ContainsAny(req.Filename, "\x00") {
		return &removeValidationError{
			message:   "filename contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if strings.ContainsAny(req.Filename, "/") {
		return &removeValidationError{
			message:   "filename contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	for i, r := range req.Filename {
		if r < 0x20 || r == 0x7F {
			return &removeValidationError{
				message:   fmt.Sprintf("filename contains control character at position %d", i),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeRemoveRequest decodes a REMOVE request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.12 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Filename length (4 bytes, big-endian uint32)
//  5. Filename data (variable length, up to 255 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the REMOVE request
//
// Returns:
//   - *RemoveRequest: The decoded request containing directory handle and filename
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded REMOVE request from network
//	req, err := DecodeRemoveRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle and req.Filename in REMOVE procedure
func DecodeRemoveRequest(data []byte) (*RemoveRequest, error) {
	// Validate minimum data length
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

	// Read directory handle length (4 bytes, big-endian)
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

	// Read directory handle
	dirHandle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &dirHandle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read handle padding byte %d: %w", i, err)
		}
	}

	// ========================================================================
	// Decode filename
	// ========================================================================

	// Read filename length (4 bytes, big-endian)
	var filenameLen uint32
	if err := binary.Read(reader, binary.BigEndian, &filenameLen); err != nil {
		return nil, fmt.Errorf("failed to read filename length: %w", err)
	}

	// Validate filename length (NFS limit is typically 255 bytes)
	if filenameLen > 255 {
		return nil, fmt.Errorf("invalid filename length: %d (max 255)", filenameLen)
	}

	// Prevent zero-length filenames
	if filenameLen == 0 {
		return nil, fmt.Errorf("invalid filename length: 0 (must be > 0)")
	}

	// Read filename
	filenameBytes := make([]byte, filenameLen)
	if err := binary.Read(reader, binary.BigEndian, &filenameBytes); err != nil {
		return nil, fmt.Errorf("failed to read filename data: %w", err)
	}

	logger.Debug("Decoded REMOVE request: handle_len=%d filename='%s'",
		handleLen, string(filenameBytes))

	return &RemoveRequest{
		DirHandle: dirHandle,
		Filename:  string(filenameBytes),
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the RemoveResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.12 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Directory WCC data (always present):
//     a. Pre-op attributes (present flag + attributes if present)
//     b. Post-op attributes (present flag + attributes if present)
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
//	resp := &RemoveResponse{
//	    Status:       types.NFS3OK,
//	    DirWccBefore: wccBefore,
//	    DirWccAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *RemoveResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write directory WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.

	if err := xdr.EncodeWccData(&buf, resp.DirWccBefore, resp.DirWccAfter); err != nil {
		return nil, fmt.Errorf("failed to encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded REMOVE response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}
