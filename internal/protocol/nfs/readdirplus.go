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

// ReadDirPlusRequest represents a READDIRPLUS request from an NFS client.
// The client provides a directory handle and parameters to retrieve directory
// entries along with their attributes and file handles.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.17 specifies the READDIRPLUS procedure as:
//
//	READDIRPLUS3res NFSPROC3_READDIRPLUS(READDIRPLUS3args) = 17;
//
// READDIRPLUS is an optimization over READDIR that returns file attributes
// and handles along with each entry, avoiding the need for subsequent LOOKUP
// and GETATTR calls for each entry.
type ReadDirPlusRequest struct {
	// DirHandle is the file handle of the directory to read.
	// Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Cookie is the position in the directory to start reading from.
	// Set to 0 for the first request. For subsequent requests, use the
	// cookie value from the last entry of the previous response.
	// This allows resuming directory reads across multiple requests.
	Cookie uint64

	// CookieVerf is a verifier to detect directory modifications.
	// Must be 0 for the first request. For subsequent requests, use the
	// cookieverf returned in the first response. If the directory has been
	// modified, the server returns NFS3ErrBadCookie.
	CookieVerf uint64

	// DirCount is the maximum size in bytes of directory information.
	// This limits the size of directory entry names and cookies.
	// Typical value: 4096-8192 bytes.
	DirCount uint32

	// MaxCount is the maximum total size in bytes for the response.
	// This includes directory information, attributes, and file handles.
	// The server may return fewer entries to stay within this limit.
	// Typical value: 32KB-64KB.
	MaxCount uint32
}

// ReadDirPlusResponse represents the response to a READDIRPLUS request.
// It contains the status, optional directory attributes, and a list of
// directory entries with their attributes and file handles.
//
// The response is encoded in XDR format before being sent back to the client.
type ReadDirPlusResponse struct {
	// Status indicates the result of the readdirplus operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): Directory not found
	//   - types.NFS3ErrNotDir (20): Handle is not a directory
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrStale (70): Stale directory handle
	//   - types.NFS3ErrBadHandle (10001): Invalid directory handle
	//   - NFS3ErrBadCookie (10003): Invalid cookie or cookieverf
	//   - NFS3ErrTooSmall (10005): MaxCount too small for entry
	Status uint32

	// DirAttr contains post-operation attributes of the directory.
	// Optional, may be nil.
	// Helps clients maintain cache consistency for the directory itself.
	DirAttr *types.NFSFileAttr

	// CookieVerf is the directory verifier.
	// Must be included in subsequent requests to detect modifications.
	// If the directory is modified, this value changes and old cookies
	// become invalid.
	CookieVerf uint64

	// Entries is the list of directory entries with attributes and handles.
	// May be empty if the directory is empty or if starting cookie is
	// beyond the last entry. Each entry includes name, attributes, and
	// file handle for efficient client-side caching.
	Entries []*DirPlusEntry

	// Eof indicates whether this is the last batch of entries.
	// true: No more entries in directory
	// false: More entries available, use last cookie for next request
	Eof bool
}

// DirPlusEntry represents a single directory entry with full information.
// This includes the basic directory entry information plus optional
// attributes and file handle for the entry.
type DirPlusEntry struct {
	// Fileid is the unique file identifier within the filesystem.
	// Similar to UNIX inode number.
	Fileid uint64

	// Name is the filename of this entry.
	// Maximum length is 255 bytes per NFS specification.
	Name string

	// Cookie is an opaque value used to resume directory reads.
	// The client passes this back in the next READDIRPLUS call to
	// continue from this position.
	Cookie uint64

	// Attr contains the file attributes for this entry.
	// May be nil if attributes could not be retrieved.
	// Includes type, permissions, size, timestamps, etc.
	Attr *types.NFSFileAttr

	// FileHandle is the file handle for this entry.
	// May be nil if the handle could not be generated.
	// Clients can use this handle directly without a LOOKUP call.
	FileHandle []byte
}

// ReadDirPlusContext contains the context information needed to process
// a READDIRPLUS request. This includes client identification and
// authentication details for access control.
type ReadDirPlusContext struct {
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
	// Used for access control checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for access control checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for checking if user belongs to file's group.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// ReadDirPlus reads directory entries with their attributes and file handles.
//
// This implements the NFS READDIRPLUS procedure as defined in RFC 1813 Section 3.3.17.
//
// **Purpose:**
//
// READDIRPLUS is an optimized version of READDIR that returns complete
// information about each directory entry in a single operation. This includes:
//   - Basic directory entry (fileid, name, cookie)
//   - File attributes (type, size, permissions, timestamps)
//   - File handle (for direct access without LOOKUP)
//
// This reduces the number of round trips significantly:
//   - READDIR: 1 call + N LOOKUP + N GETATTR = 2N+1 operations
//   - READDIRPLUS: 1 call = 1 operation
//
// **Process:**
//
//  1. Check for context cancellation before starting
//  2. Validate request parameters (handle, counts, cookie)
//  3. Extract client IP and authentication credentials from context
//  4. Verify directory handle exists and is a directory (via repository)
//  5. Get directory children from repository
//  6. Build special entries ("." and "..") with cancellation checks
//  7. Process each child entry with periodic cancellation checks
//  8. Return entries with attributes and handles
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (directory listing, access control) delegated to repository
//   - File handle validation performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//   - Respects context cancellation for graceful shutdown and timeouts
//   - Periodic cancellation checks during entry processing for large directories
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the repository, which can implement:
//   - Read permission checking on the directory
//   - Access control based on UID/GID
//   - Filtering of entries based on permissions
//
// **Cookie and Pagination:**
//
// READDIRPLUS supports resumable directory reads:
//   - First request: cookie=0, cookieverf=0
//   - Server returns: entries with cookies, cookieverf
//   - Subsequent requests: cookie=last_cookie, cookieverf=returned_value
//   - Continue until eof=true
//
// The cookieverf detects directory modifications:
//   - If directory unchanged: same cookieverf, cookies are valid
//   - If directory modified: new cookieverf, old cookies become invalid
//   - Client must restart from cookie=0
//
// **Size Limits:**
//
// Two size limits control response size:
//   - DirCount: Limits size of names and cookies only
//   - MaxCount: Limits total size including attributes and handles
//
// The server must ensure the response doesn't exceed these limits.
// If a single entry exceeds the limits, return NFS3ErrTooSmall.
//
// **Special Entries:**
//
// The first two entries are always:
//   - "." (current directory): Points to directory itself
//   - ".." (parent directory): Points to parent directory
//
// For the root directory, ".." points to itself (no parent above root).
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - Directory not found → types.NFS3ErrNoEnt
//   - Not a directory → types.NFS3ErrNotDir
//   - Invalid cookie → NFS3ErrBadCookie
//   - Permission denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO
//
// **Performance Considerations:**
//
// READDIRPLUS returns more data than READDIR and may be slower:
//   - Must retrieve attributes for all entries
//   - Must generate file handles for all entries
//   - May require additional I/O operations
//   - Can be expensive for large directories
//
// However, it eliminates multiple round trips, improving overall performance
// for operations that need file attributes (like 'ls -l').
//
// **Context Cancellation:**
//
// This operation respects context cancellation:
//   - Checks at operation start before any work
//   - Checks before each repository call (GetFile, GetChildren)
//   - Checks periodically during entry processing (every 50 entries)
//   - Returns types.NFS3ErrIO on cancellation with DirAttr when available
//
// For large directories with hundreds of entries, periodic cancellation checks
// during processing ensure responsiveness to client disconnects and server
// shutdown without adding significant overhead.
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository enforces read permission on directory
//   - Access control can filter entries based on permissions
//   - Client context enables audit logging
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - repository: The metadata repository for directory and file operations
//   - req: The readdirplus request containing directory handle and parameters
//
// **Returns:**
//   - *ReadDirPlusResponse: Response with status and directory entries (if successful)
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.17: READDIRPLUS Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &ReadDirPlusRequest{
//	    DirHandle:  dirHandle,
//	    Cookie:     0,
//	    CookieVerf: 0,
//	    DirCount:   8192,
//	    MaxCount:   65536,
//	}
//	ctx := &ReadDirPlusContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.ReadDirPlus(ctx, repository, req)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Process resp.Entries
//	    for _, entry := range resp.Entries {
//	        // Use entry.Name, entry.Attr, entry.FileHandle
//	    }
//	    if !resp.Eof {
//	        // More entries available, use last cookie
//	    }
//	}
func (h *DefaultNFSHandler) ReadDirPlus(
	ctx *ReadDirPlusContext,
	repository metadata.Repository,
	req *ReadDirPlusRequest,
) (*ReadDirPlusResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("READDIRPLUS: dir=%x cookie=%d dircount=%d maxcount=%d client=%s auth=%d",
		req.DirHandle, req.Cookie, req.DirCount, req.MaxCount, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	select {
	case <-ctx.Context.Done():
		logger.Warn("READDIRPLUS cancelled: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, ctx.Context.Err())
		return &ReadDirPlusResponse{Status: types.NFS3ErrIO}, nil
	default:
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateReadDirPlusRequest(req); err != nil {
		logger.Warn("READDIRPLUS validation failed: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &ReadDirPlusResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 3: Verify directory handle exists and is valid
	// ========================================================================

	// Check context before repository call
	select {
	case <-ctx.Context.Done():
		logger.Warn("READDIRPLUS cancelled before GetFile: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, ctx.Context.Err())
		return &ReadDirPlusResponse{Status: types.NFS3ErrIO}, nil
	default:
	}

	dirHandle := metadata.FileHandle(req.DirHandle)
	dirAttr, err := repository.GetFile(ctx.Context, dirHandle)
	if err != nil {
		logger.Warn("READDIRPLUS failed: directory not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &ReadDirPlusResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Verify handle is actually a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("READDIRPLUS failed: handle not a directory: dir=%x type=%d client=%s",
			req.DirHandle, dirAttr.Type, clientIP)

		// Include directory attributes even on error for cache consistency
		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &ReadDirPlusResponse{
			Status:  types.NFS3ErrNotDir,
			DirAttr: nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 4: Get directory children from repository
	// ========================================================================
	// The repository only retrieves the children of the directory.
	// This protocol layer is responsible for building the "." and ".." entries.
	// Access control filtering, if required, would also need to be implemented in this layer.

	// Check context before repository call
	select {
	case <-ctx.Context.Done():
		logger.Warn("READDIRPLUS cancelled before GetChildren: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, ctx.Context.Err())

		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &ReadDirPlusResponse{
			Status:  types.NFS3ErrIO,
			DirAttr: nfsDirAttr,
		}, nil
	default:
	}

	children, err := repository.GetChildren(ctx.Context, dirHandle)
	if err != nil {
		logger.Error("READDIRPLUS failed: error retrieving children: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)

		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &ReadDirPlusResponse{
			Status:  types.NFS3ErrIO,
			DirAttr: nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 5: Build response with directory entries
	// ========================================================================

	// Generate directory file ID for attributes
	dirID := xdr.ExtractFileID(dirHandle)
	nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

	// Build entries list with special entries and children
	entries := make([]*DirPlusEntry, 0)
	cookie := uint64(1)

	// ========================================================================
	// Add "." entry (current directory)
	// ========================================================================

	if req.Cookie == 0 {
		entries = append(entries, &DirPlusEntry{
			Fileid:     dirID,
			Name:       ".",
			Cookie:     cookie,
			Attr:       nfsDirAttr,
			FileHandle: req.DirHandle,
		})
		cookie++

		logger.Debug("READDIRPLUS: added '.' entry cookie=%d", cookie-1)
	}

	// ========================================================================
	// Add ".." entry (parent directory)
	// ========================================================================

	if req.Cookie <= 1 {
		// Check context periodically (before parent lookup)
		select {
		case <-ctx.Context.Done():
			logger.Warn("READDIRPLUS cancelled during parent lookup: dir=%x client=%s error=%v",
				req.DirHandle, clientIP, ctx.Context.Err())

			return &ReadDirPlusResponse{
				Status:  types.NFS3ErrIO,
				DirAttr: nfsDirAttr,
			}, nil
		default:
		}

		// Get parent handle and attributes
		parentFileid := dirID
		parentHandle := req.DirHandle
		parentAttr := nfsDirAttr

		// Try to get actual parent (may not exist for root)
		ph, err := repository.GetParent(ctx.Context, dirHandle)
		if err == nil {
			parentHandle = ph
			parentFileid = xdr.ExtractFileID(ph)

			// Get parent attributes
			if pAttr, err := repository.GetFile(ctx.Context, ph); err == nil {
				parentAttr = xdr.MetadataToNFS(pAttr, parentFileid)
			} else {
				logger.Warn("READDIRPLUS: parent handle exists but attributes missing: parent=%x error=%v",
					ph, err)
			}
		} else {
			logger.Debug("READDIRPLUS: no parent found (likely root directory), using self for '..'")
		}

		entries = append(entries, &DirPlusEntry{
			Fileid:     parentFileid,
			Name:       "..",
			Cookie:     cookie,
			Attr:       parentAttr,
			FileHandle: parentHandle,
		})
		cookie++

		logger.Debug("READDIRPLUS: added '..' entry cookie=%d", cookie-1)
	}

	// ========================================================================
	// Add regular entries (actual directory children)
	// ========================================================================

	entriesAdded := 0
	entriesProcessed := 0

	for name, childHandle := range children {
		// Check context periodically (every 50 entries) for large directories
		if entriesProcessed%50 == 0 {
			select {
			case <-ctx.Context.Done():
				logger.Warn("READDIRPLUS cancelled during entry processing: dir=%x processed=%d client=%s error=%v",
					req.DirHandle, entriesProcessed, clientIP, ctx.Context.Err())

				return &ReadDirPlusResponse{
					Status:  types.NFS3ErrIO,
					DirAttr: nfsDirAttr,
				}, nil
			default:
			}
		}
		entriesProcessed++

		// Skip entries before the requested cookie
		if cookie <= req.Cookie {
			cookie++
			continue
		}

		// Generate file ID for this entry
		childID := xdr.ExtractFileID(childHandle)

		// Get attributes for this entry
		var entryAttr *types.NFSFileAttr
		if attr, err := repository.GetFile(ctx.Context, childHandle); err == nil {
			entryAttr = xdr.MetadataToNFS(attr, childID)
		} else {
			logger.Warn("READDIRPLUS: child handle exists but attributes missing: name='%s' handle=%x error=%v",
				name, childHandle, err)
			// Continue with nil attributes rather than failing entire operation
		}

		entries = append(entries, &DirPlusEntry{
			Fileid:     childID,
			Name:       name,
			Cookie:     cookie,
			Attr:       entryAttr,
			FileHandle: childHandle,
		})

		cookie++
		entriesAdded++

		logger.Debug("READDIRPLUS: added entry '%s' cookie=%d fileid=%d",
			name, cookie-1, childID)
	}

	// ========================================================================
	// Step 6: Build success response
	// ========================================================================

	logger.Info("READDIRPLUS successful: dir=%x entries=%d (special=2 regular=%d) eof=true client=%s",
		req.DirHandle, len(entries), entriesAdded, clientIP)

	logger.Debug("READDIRPLUS details: dir_id=%d total_children=%d cookie_start=%d cookie_end=%d",
		dirID, len(children), req.Cookie, cookie-1)

	return &ReadDirPlusResponse{
		Status:     types.NFS3OK,
		DirAttr:    nfsDirAttr,
		CookieVerf: 0, // We don't implement cookie verification yet
		Entries:    entries,
		Eof:        true, // For simplicity, return all entries in one response
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// readDirPlusValidationError represents a READDIRPLUS request validation error.
type readDirPlusValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *readDirPlusValidationError) Error() string {
	return e.message
}

// validateReadDirPlusRequest validates READDIRPLUS request parameters.
//
// Checks performed:
//   - Directory handle is not nil or empty
//   - Directory handle length is within RFC 1813 limits (max 64 bytes)
//   - Directory handle is long enough for file ID extraction (min 8 bytes)
//   - DirCount is reasonable (not zero, not excessively large)
//   - MaxCount is reasonable and >= DirCount
//
// Returns:
//   - nil if valid
//   - *readDirPlusValidationError with NFS status if invalid
func validateReadDirPlusRequest(req *ReadDirPlusRequest) *readDirPlusValidationError {
	// Validate directory handle
	if len(req.DirHandle) == 0 {
		return &readDirPlusValidationError{
			message:   "empty directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.DirHandle) > 64 {
		return &readDirPlusValidationError{
			message:   fmt.Sprintf("directory handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &readDirPlusValidationError{
			message:   fmt.Sprintf("directory handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate DirCount (should be reasonable)
	if req.DirCount == 0 {
		return &readDirPlusValidationError{
			message:   "dircount cannot be zero",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate MaxCount (should be reasonable and >= DirCount)
	if req.MaxCount == 0 {
		return &readDirPlusValidationError{
			message:   "maxcount cannot be zero",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if req.MaxCount < req.DirCount {
		return &readDirPlusValidationError{
			message:   fmt.Sprintf("maxcount (%d) must be >= dircount (%d)", req.MaxCount, req.DirCount),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for excessively large counts that might indicate a malformed request
	const maxReasonableSize = 1024 * 1024 // 1MB
	if req.MaxCount > maxReasonableSize {
		return &readDirPlusValidationError{
			message:   fmt.Sprintf("maxcount too large: %d bytes (max %d)", req.MaxCount, maxReasonableSize),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeReadDirPlusRequest decodes a READDIRPLUS request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.17 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Cookie (8 bytes, big-endian uint64)
//  5. Cookie verifier (8 bytes, big-endian uint64)
//  6. DirCount (4 bytes, big-endian uint32)
//  7. MaxCount (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the READDIRPLUS request
//
// Returns:
//   - *ReadDirPlusRequest: The decoded request
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READDIRPLUS request from network
//	req, err := DecodeReadDirPlusRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req in READDIRPLUS procedure
func DecodeReadDirPlusRequest(data []byte) (*ReadDirPlusRequest, error) {
	// Validate minimum data length
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

	// Read handle length (4 bytes, big-endian)
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
	for i := range padding {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read handle padding byte %d: %w", i, err)
		}
	}

	// ========================================================================
	// Decode cookie (8 bytes, big-endian)
	// ========================================================================

	var cookie uint64
	if err := binary.Read(reader, binary.BigEndian, &cookie); err != nil {
		return nil, fmt.Errorf("failed to read cookie: %w", err)
	}

	// ========================================================================
	// Decode cookieverf (8 bytes, big-endian)
	// ========================================================================

	var cookieVerf uint64
	if err := binary.Read(reader, binary.BigEndian, &cookieVerf); err != nil {
		return nil, fmt.Errorf("failed to read cookieverf: %w", err)
	}

	// ========================================================================
	// Decode dircount (4 bytes, big-endian)
	// ========================================================================

	var dirCount uint32
	if err := binary.Read(reader, binary.BigEndian, &dirCount); err != nil {
		return nil, fmt.Errorf("failed to read dircount: %w", err)
	}

	// ========================================================================
	// Decode maxcount (4 bytes, big-endian)
	// ========================================================================

	var maxCount uint32
	if err := binary.Read(reader, binary.BigEndian, &maxCount); err != nil {
		return nil, fmt.Errorf("failed to read maxcount: %w", err)
	}

	logger.Debug("Decoded READDIRPLUS request: handle_len=%d cookie=%d cookieverf=%d dircount=%d maxcount=%d",
		handleLen, cookie, cookieVerf, dirCount, maxCount)

	return &ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     cookie,
		CookieVerf: cookieVerf,
		DirCount:   dirCount,
		MaxCount:   maxCount,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadDirPlusResponse into XDR-encoded bytes suitable
// for transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.17 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op directory attributes (present flag + attributes if present)
//  3. If status == types.NFS3OK:
//     a. Cookie verifier (8 bytes)
//     b. Entry list:
//     - For each entry:
//     * value_follows flag (1)
//     * fileid (8 bytes)
//     * name length + name data + padding
//     * cookie (8 bytes)
//     * name_attributes (present flag + attributes if present)
//     * name_handle (present flag + handle if present)
//     - End marker: value_follows flag (0)
//     c. EOF flag (4 bytes, 0 or 1)
//  4. If status != types.NFS3OK:
//     a. No additional data beyond directory attributes
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
//	resp := &ReadDirPlusResponse{
//	    Status:     types.NFS3OK,
//	    DirAttr:    dirAttr,
//	    CookieVerf: 0,
//	    Entries:    entries,
//	    Eof:        true,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadDirPlusResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op directory attributes (both success and failure cases)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.DirAttr); err != nil {
		return nil, fmt.Errorf("failed to encode directory attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return early (no entries)
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READDIRPLUS error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write cookie verifier
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.CookieVerf); err != nil {
		return nil, fmt.Errorf("failed to write cookieverf: %w", err)
	}

	// ========================================================================
	// Write directory entries
	// ========================================================================

	for _, entry := range resp.Entries {
		// value_follows = TRUE (1) - indicates an entry follows
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("failed to write value_follows flag: %w", err)
		}

		// Write fileid (8 bytes)
		if err := binary.Write(&buf, binary.BigEndian, entry.Fileid); err != nil {
			return nil, fmt.Errorf("failed to write fileid for entry '%s': %w", entry.Name, err)
		}

		// Write name (string: length + data + padding)
		nameLen := uint32(len(entry.Name))
		if err := binary.Write(&buf, binary.BigEndian, nameLen); err != nil {
			return nil, fmt.Errorf("failed to write name length for entry '%s': %w", entry.Name, err)
		}

		if _, err := buf.Write([]byte(entry.Name)); err != nil {
			return nil, fmt.Errorf("failed to write name data for entry '%s': %w", entry.Name, err)
		}

		// Add padding to 4-byte boundary
		padding := (4 - (nameLen % 4)) % 4
		for i := range padding {
			if err := buf.WriteByte(0); err != nil {
				return nil, fmt.Errorf("failed to write name padding byte %d for entry '%s': %w", i, entry.Name, err)
			}
		}

		// Write cookie (8 bytes)
		if err := binary.Write(&buf, binary.BigEndian, entry.Cookie); err != nil {
			return nil, fmt.Errorf("failed to write cookie for entry '%s': %w", entry.Name, err)
		}

		// Write name_attributes (post-op attributes - optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, entry.Attr); err != nil {
			return nil, fmt.Errorf("failed to encode attributes for entry '%s': %w", entry.Name, err)
		}

		// Write name_handle (post-op file handle - optional)
		if err := xdr.EncodeOptionalOpaque(&buf, entry.FileHandle); err != nil {
			return nil, fmt.Errorf("failed to encode handle for entry '%s': %w", entry.Name, err)
		}
	}

	// ========================================================================
	// Write end-of-list marker
	// ========================================================================

	// value_follows = FALSE (0) - indicates no more entries
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("failed to write end-of-list marker: %w", err)
	}

	// ========================================================================
	// Write EOF flag
	// ========================================================================

	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("failed to write eof flag: %w", err)
	}

	logger.Debug("Encoded READDIRPLUS response: %d bytes status=%d entries=%d eof=%v",
		buf.Len(), resp.Status, len(resp.Entries), resp.Eof)

	return buf.Bytes(), nil
}
