package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// LinkRequest represents a LINK request from an NFS client.
// The LINK procedure creates a hard link to an existing file.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.15 specifies the LINK procedure as:
//
//	LINK3res NFSPROC3_LINK(LINK3args) = 15;
//
// Hard links create additional directory entries that reference the same
// underlying file. All hard links to a file are equivalent - there is no
// "original" and modifications through any link affect all links.
type LinkRequest struct {
	// FileHandle is the file handle of the existing file to link to.
	// This must be a valid file handle for a regular file, not a directory.
	// Maximum length is 64 bytes per RFC 1813.
	FileHandle []byte

	// DirHandle is the file handle of the directory where the new link will be created.
	// This must be a valid directory handle.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Name is the name for the new link within the target directory.
	// Must follow NFS naming conventions (max 255 bytes, no null bytes or slashes).
	// Must not already exist in the target directory.
	Name string
}

// LinkResponse represents the response to a LINK request.
// It contains the status of the operation and, if successful, post-operation
// attributes for both the linked file and the target directory.
//
// The response is encoded in XDR format before being sent back to the client.
type LinkResponse struct {
	// Status indicates the result of the link operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): Source file or target directory not found
	//   - NFS3ErrExist (17): Name already exists in target directory
	//   - types.NFS3ErrNotDir (20): DirHandle is not a directory
	//   - types.NFS3ErrIsDir (21): Attempted to link a directory
	//   - NFS3ErrInval (22): Invalid argument
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

	// FileAttr contains post-operation attributes of the linked file.
	// Only present when Status == types.NFS3OK or for cache consistency on errors.
	// The nlink count will be incremented to reflect the new hard link.
	FileAttr *types.NFSFileAttr

	// DirWccBefore contains pre-operation attributes of the target directory.
	// Used for weak cache consistency to help clients detect changes.
	DirWccBefore *types.WccAttr

	// DirWccAfter contains post-operation attributes of the target directory.
	// Used for weak cache consistency. Present for both success and failure.
	DirWccAfter *types.NFSFileAttr
}

// LinkContext contains the context information needed to process a LINK request.
// This includes client identification and authentication details for access control.
type LinkContext struct {
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
	// Used for access control checks.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for access control checks.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for access control checks.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Link creates a hard link to an existing file.
//
// This implements the NFS LINK procedure as defined in RFC 1813 Section 3.3.15.
//
// **Purpose:**
//
// Hard links create additional directory entries that point to the same file.
// Unlike symbolic links:
//   - Hard links reference the same inode/file data
//   - All links are equivalent (no "original")
//   - Deleting one link doesn't affect others
//   - Links must be on the same filesystem
//   - Cannot link directories (to prevent cycles)
//
// **Process:**
//
//  1. Check for context cancellation early
//  2. Validate request parameters (handles, name)
//  3. Extract client IP and authentication credentials
//  4. Verify source file exists and is a regular file (not a directory)
//  5. Verify target directory exists and is a directory
//  6. Capture pre-operation directory state (for WCC)
//  7. Check that the link name doesn't already exist
//  8. Delegate link creation to repository.CreateLink()
//  9. Return file attributes and directory WCC data
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (link creation, validation) is delegated to repository
//   - File handle validation is performed by repository.GetFile()
//   - Context cancellation is checked at strategic points between operations
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Hard Link Restrictions:**
//
// Per RFC 1813 and standard Unix semantics:
//   - Cannot create hard links to directories (prevents filesystem cycles)
//   - Cannot create hard links across different filesystems
//   - Link count (nlink) is incremented for the target file
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// Access control should be enforced by the repository layer based on:
//   - Write permission on the target directory
//   - Client credentials (UID/GID)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - Source not found → types.NFS3ErrNoEnt
//   - Target directory not found → types.NFS3ErrNoEnt
//   - Name already exists → NFS3ErrExist
//   - Source is directory → types.NFS3ErrIsDir
//   - Target not directory → types.NFS3ErrNotDir
//   - Access denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository enforces write access to target directory
//   - Cannot link directories (prevents privilege escalation)
//   - Client context enables audit logging
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - repository: The metadata repository for file and link operations
//   - req: The link request containing file handle, directory, and name
//
// **Returns:**
//   - *LinkResponse: Response with status and attributes (if successful)
//   - error: Returns error only for catastrophic internal failures or context
//     cancellation; protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.15: LINK Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &LinkRequest{
//	    FileHandle: sourceHandle,
//	    DirHandle:  targetDirHandle,
//	    Name:       "hardlink.txt",
//	}
//	ctx := &LinkContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Link(ctx, repository, req)
//	if err != nil {
//	    // Internal server error or context cancellation
//	}
//	if resp.Status == types.NFS3OK {
//	    // Hard link created successfully
//	}
func (h *DefaultNFSHandler) Link(
	ctx *LinkContext,
	repository metadata.Repository,
	req *LinkRequest,
) (*LinkResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("LINK: file=%x to '%s' in dir=%x client=%s auth=%d",
		req.FileHandle, req.Name, req.DirHandle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation early
	// ========================================================================
	// LINK involves multiple operations, so respect cancellation to avoid
	// wasting resources on abandoned requests

	select {
	case <-ctx.Context.Done():
		logger.Debug("LINK cancelled: file=%x name='%s' client=%s error=%v",
			req.FileHandle, req.Name, clientIP, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateLinkRequest(req); err != nil {
		logger.Warn("LINK validation failed: name='%s' client=%s error=%v",
			req.Name, clientIP, err)
		return &LinkResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 3: Check cancellation before first repository operation
	// ========================================================================

	select {
	case <-ctx.Context.Done():
		logger.Debug("LINK cancelled before GetFile: file=%x name='%s' client=%s error=%v",
			req.FileHandle, req.Name, clientIP, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 4: Verify source file exists and is a regular file
	// ========================================================================

	fileHandle := metadata.FileHandle(req.FileHandle)
	fileAttr, err := repository.GetFile(ctx.Context, fileHandle)
	if err != nil {
		logger.Warn("LINK failed: source file not found: file=%x client=%s error=%v",
			req.FileHandle, clientIP, err)
		return &LinkResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Hard links to directories are not allowed (prevents filesystem cycles)
	if fileAttr.Type == metadata.FileTypeDirectory {
		logger.Warn("LINK failed: cannot link directory: file=%x client=%s",
			req.FileHandle, clientIP)
		return &LinkResponse{Status: types.NFS3ErrIsDir}, nil
	}

	// ========================================================================
	// Step 5: Check cancellation before target directory lookup
	// ========================================================================

	select {
	case <-ctx.Context.Done():
		logger.Debug("LINK cancelled before directory lookup: file=%x name='%s' client=%s error=%v",
			req.FileHandle, req.Name, clientIP, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 6: Verify target directory exists and is a directory
	// ========================================================================

	dirHandle := metadata.FileHandle(req.DirHandle)
	dirAttr, err := repository.GetFile(ctx.Context, dirHandle)
	if err != nil {
		logger.Warn("LINK failed: target directory not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &LinkResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation directory attributes for WCC
	dirWccBefore := xdr.CaptureWccAttr(dirAttr)

	// Verify target is a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("LINK failed: target not a directory: dir=%x type=%d client=%s",
			req.DirHandle, dirAttr.Type, clientIP)

		// Get current directory state for WCC
		dirID := xdr.ExtractFileID(dirHandle)
		dirWccAfter := xdr.MetadataToNFS(dirAttr, dirID)

		return &LinkResponse{
			Status:       types.NFS3ErrNotDir,
			DirWccBefore: dirWccBefore,
			DirWccAfter:  dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 7: Check cancellation before name conflict check
	// ========================================================================

	select {
	case <-ctx.Context.Done():
		logger.Debug("LINK cancelled before name check: file=%x name='%s' client=%s error=%v",
			req.FileHandle, req.Name, clientIP, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 8: Check if name already exists in target directory
	// ========================================================================

	_, err = repository.GetChild(ctx.Context, dirHandle, req.Name)
	if err == nil {
		logger.Debug("LINK failed: name already exists: name='%s' dir=%x client=%s",
			req.Name, req.DirHandle, clientIP)

		// Get updated directory attributes for WCC
		dirAttr, _ = repository.GetFile(ctx.Context, dirHandle)
		dirID := xdr.ExtractFileID(dirHandle)
		dirWccAfter := xdr.MetadataToNFS(dirAttr, dirID)

		return &LinkResponse{
			Status:       types.NFS3ErrExist,
			DirWccBefore: dirWccBefore,
			DirWccAfter:  dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 9: Check cancellation before write operation
	// ========================================================================
	// This is the most critical check as CreateLink modifies filesystem state

	select {
	case <-ctx.Context.Done():
		logger.Debug("LINK cancelled before CreateLink: file=%x name='%s' client=%s error=%v",
			req.FileHandle, req.Name, clientIP, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 10: Create the hard link via repository
	// ========================================================================
	// The repository is responsible for:
	// - Verifying write access to the target directory
	// - Adding the new directory entry
	// - Incrementing the link count (nlink) on the file
	// - Updating directory timestamps

	// Build authentication context for repository
	authCtx := &metadata.AuthContext{
		AuthFlavor: ctx.AuthFlavor,
		UID:        ctx.UID,
		GID:        ctx.GID,
		GIDs:       ctx.GIDs,
		ClientAddr: clientIP,
	}

	err = repository.CreateLink(authCtx, dirHandle, req.Name, fileHandle)
	if err != nil {
		logger.Error("LINK failed: repository error: name='%s' client=%s error=%v",
			req.Name, clientIP, err)

		// Get updated directory attributes for WCC
		dirAttr, _ = repository.GetFile(ctx.Context, dirHandle)
		dirID := xdr.ExtractFileID(dirHandle)
		dirWccAfter := xdr.MetadataToNFS(dirAttr, dirID)

		// Map repository errors to NFS status codes
		status := xdr.MapRepositoryErrorToNFSStatus(err, clientIP, "link")

		return &LinkResponse{
			Status:       status,
			DirWccBefore: dirWccBefore,
			DirWccAfter:  dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 11: Build success response with updated attributes
	// ========================================================================
	// No cancellation check here - operation succeeded, fetching attributes
	// is best-effort for cache consistency

	// Get updated file attributes (nlink should be incremented)
	fileAttr, err = repository.GetFile(ctx.Context, fileHandle)
	if err != nil {
		logger.Error("LINK: failed to get file attributes after link: file=%x error=%v",
			req.FileHandle, err)
		// Continue with cached attributes
	}

	fileID := xdr.ExtractFileID(fileHandle)
	nfsFileAttr := xdr.MetadataToNFS(fileAttr, fileID)

	// Get updated directory attributes
	dirAttr, _ = repository.GetFile(ctx.Context, dirHandle)
	dirID := xdr.ExtractFileID(dirHandle)
	nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

	logger.Info("LINK successful: name='%s' file=%x nlink=%d client=%s",
		req.Name, req.FileHandle, nfsFileAttr.Nlink, clientIP)

	return &LinkResponse{
		Status:       types.NFS3OK,
		FileAttr:     nfsFileAttr,
		DirWccBefore: dirWccBefore,
		DirWccAfter:  nfsDirAttr,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// linkValidationError represents a LINK request validation error.
type linkValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *linkValidationError) Error() string {
	return e.message
}

// validateLinkRequest validates LINK request parameters.
//
// Checks performed:
//   - Source file handle is not empty and within limits
//   - Target directory handle is not empty and within limits
//   - Link name is valid (not empty, length, characters)
//
// Returns:
//   - nil if valid
//   - *linkValidationError with NFS status if invalid
func validateLinkRequest(req *LinkRequest) *linkValidationError {
	// Validate source file handle
	if len(req.FileHandle) == 0 {
		return &linkValidationError{
			message:   "empty source file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.FileHandle) > 64 {
		return &linkValidationError{
			message:   fmt.Sprintf("source file handle too long: %d bytes (max 64)", len(req.FileHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate target directory handle
	if len(req.DirHandle) == 0 {
		return &linkValidationError{
			message:   "empty directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &linkValidationError{
			message:   fmt.Sprintf("directory handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate link name
	if req.Name == "" {
		return &linkValidationError{
			message:   "empty link name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if len(req.Name) > 255 {
		return &linkValidationError{
			message:   fmt.Sprintf("link name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for invalid characters
	if bytes.ContainsAny([]byte(req.Name), "/\x00") {
		return &linkValidationError{
			message:   "link name contains invalid characters (null or path separator)",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &linkValidationError{
			message:   fmt.Sprintf("link name cannot be '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeLinkRequest decodes a LINK request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.15 specifications:
//  1. Source file handle length (4 bytes, big-endian uint32)
//  2. Source file handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Target directory handle length (4 bytes, big-endian uint32)
//  5. Target directory handle data (variable length, up to 64 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//  7. Link name length (4 bytes, big-endian uint32)
//  8. Link name data (variable length, up to 255 bytes)
//  9. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the LINK request
//
// Returns:
//   - *LinkRequest: The decoded request containing file handle, directory, and name
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded LINK request from network
//	req, err := DecodeLinkRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.FileHandle, req.DirHandle, req.Name in LINK procedure
func DecodeLinkRequest(data []byte) (*LinkRequest, error) {
	// Validate minimum data length
	if len(data) < 12 {
		return nil, fmt.Errorf("data too short: need at least 12 bytes for handles, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode source file handle
	// ========================================================================

	fileHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode file handle: %w", err)
	}

	// ========================================================================
	// Decode target directory handle
	// ========================================================================

	dirHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}

	// ========================================================================
	// Decode link name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}

	logger.Debug("Decoded LINK request: file_handle_len=%d dir_handle_len=%d name='%s'",
		len(fileHandle), len(dirHandle), name)

	return &LinkRequest{
		FileHandle: fileHandle,
		DirHandle:  dirHandle,
		Name:       name,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the LinkResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.15 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op file attributes (present flag + attributes if present)
//  3. Directory WCC data (pre-op and post-op attributes)
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
//	resp := &LinkResponse{
//	    Status:       types.NFS3OK,
//	    FileAttr:     fileAttr,
//	    DirWccBefore: wccBefore,
//	    DirWccAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *LinkResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write post-op file attributes (optional)
	// ========================================================================
	// Present for both success and failure cases to help clients
	// maintain cache consistency

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.FileAttr); err != nil {
		return nil, fmt.Errorf("encode file attributes: %w", err)
	}

	// ========================================================================
	// Write directory WCC data (always present)
	// ========================================================================
	// Weak cache consistency data helps clients detect if the directory
	// changed during the operation

	if err := xdr.EncodeWccData(&buf, resp.DirWccBefore, resp.DirWccAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded LINK response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}
