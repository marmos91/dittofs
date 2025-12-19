package handlers

import (
	"fmt"
	"strconv"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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
	NFSResponseBase // Embeds Status field and GetStatus() method

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
//  4. Verify directory handle exists and is a directory (via store)
//  5. Get directory children from store
//  6. Build special entries ("." and "..") with cancellation checks
//  7. Process each child entry with periodic cancellation checks
//  8. Return entries with attributes and handles
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (directory listing, access control) delegated to store
//   - File handle validation performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//   - Respects context cancellation for graceful shutdown and timeouts
//   - Periodic cancellation checks during entry processing for large directories
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the store, which can implement:
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
// store errors are mapped to NFS status codes:
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
//   - Checks before each store call (GetFile, GetChildren)
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
//   - store enforces read permission on directory
//   - Access control can filter entries based on permissions
//   - Client context enables audit logging
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - metadataStore: The metadata store for directory and file operations
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
//	resp, err := handler.ReadDirPlus(ctx, store, req)
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
func (h *Handler) ReadDirPlus(
	ctx *NFSHandlerContext,
	req *ReadDirPlusRequest,
) (*ReadDirPlusResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "READDIRPLUS", "handle", fmt.Sprintf("%x", req.DirHandle), "cookie", req.Cookie, "dircount", req.DirCount, "maxcount", req.MaxCount, "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "READDIRPLUS cancelled", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())
		return &ReadDirPlusResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateReadDirPlusRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "READDIRPLUS validation failed", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", err)
		return &ReadDirPlusResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 3: Verify directory handle exists and is valid
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "READDIRPLUS cancelled before GetFile", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())
		return &ReadDirPlusResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Get metadata store from context
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(ctx.Share)
	if err != nil {
		logger.WarnCtx(ctx.Context, "READDIRPLUS failed", "error", err, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)
		return &ReadDirPlusResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	dirHandle := metadata.FileHandle(req.DirHandle)

	logger.DebugCtx(ctx.Context, "READDIRPLUS", "share", ctx.Share)

	dirFile, err := metadataStore.GetFile(ctx.Context, dirHandle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "READDIRPLUS failed: directory not found", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", err)
		return &ReadDirPlusResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	// Verify handle is actually a directory
	if dirFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "READDIRPLUS failed: handle not a directory", "handle", fmt.Sprintf("%x", req.DirHandle), "type", dirFile.Type, "client", clientIP)

		// Include directory attributes even on error for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &ReadDirPlusResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Cookie Verifier Validation (RFC 1813 Section 3.3.17)
	// ========================================================================
	// Generate verifier from directory mtime - changes when directory is modified
	currentVerifier := directoryMtimeVerifier(dirFile.Mtime)

	// Validate cookie verifier for non-initial requests
	// Initial request (cookie=0) or clients that don't use verifiers (verifier=0) bypass this check
	if req.Cookie != 0 && req.CookieVerf != 0 && req.CookieVerf != currentVerifier {
		logger.WarnCtx(ctx.Context, "READDIRPLUS: directory modified since last read",
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"expected_verf", fmt.Sprintf("0x%016x", req.CookieVerf),
			"current_verf", fmt.Sprintf("0x%016x", currentVerifier),
			"client", clientIP)

		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)
		return &ReadDirPlusResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadCookie},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 4: Build authentication context for store
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "READDIRPLUS cancelled during auth context building", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

			nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

			return &ReadDirPlusResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				DirAttr:         nfsDirAttr,
			}, nil
		}

		traceError(ctx.Context, err, "READDIRPLUS failed: failed to build auth context", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &ReadDirPlusResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 5: Get directory entries from store
	// ========================================================================
	// The store retrieves the directory entries via ReadDirectory.
	// We need to use Lookup for each entry to get handles and full attributes.

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "READDIRPLUS cancelled before ReadDirectory", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &ReadDirPlusResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// Convert cookie to string token for ReadDirectory pagination
	// Cookie represents the position to resume from (0 = start from beginning)
	token := ""
	if req.Cookie > 0 {
		token = strconv.FormatUint(req.Cookie, 10)
	}

	// Use DirCount as maxBytes hint for ReadDirectory
	// ReadDirectory handles retries internally to ensure consistent snapshots
	page, err := metadataStore.ReadDirectory(authCtx, dirHandle, token, req.DirCount)
	if err != nil {
		traceError(ctx.Context, err, "READDIRPLUS failed: error retrieving entries", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

		// Map store error to NFS status
		status := mapMetadataErrorToNFS(err)

		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &ReadDirPlusResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 6: Build response with directory entries
	// ========================================================================

	// Generate directory file ID for attributes
	nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

	// Build entries list - look up each entry to get handle and full attributes
	entries := make([]*DirPlusEntry, 0, len(page.Entries))

	// Starting offset for cookie calculation (cookies must be absolute positions)
	startOffset := req.Cookie

	for i, entry := range page.Entries {
		// Check context periodically (every 50 entries) for large directories
		if i%50 == 0 {
			select {
			case <-ctx.Context.Done():
				logger.WarnCtx(ctx.Context, "READDIRPLUS cancelled during entry processing", "handle", fmt.Sprintf("%x", req.DirHandle), "processed", i, "client", clientIP, "error", ctx.Context.Err())

				return &ReadDirPlusResponse{
					NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
					DirAttr:         nfsDirAttr,
				}, nil
			default:
			}
		}

		// Use Handle from ReadDirectory result to avoid expensive Lookup() calls
		// The Handle field is populated by ReadDirectory (see pkg/metadata/directory.go)
		entryHandle := entry.Handle
		if len(entryHandle) == 0 {
			// Fallback: Handle not populated, use Lookup (shouldn't happen with proper implementation)
			logger.WarnCtx(ctx.Context, "READDIRPLUS: entry.Handle not populated, falling back to Lookup", "name", entry.Name)
			var err error
			lookupFile, err := metadataStore.Lookup(authCtx, dirHandle, entry.Name)
			if err != nil {
				logger.WarnCtx(ctx.Context, "READDIRPLUS: failed to lookup", "name", entry.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "error", err)
				// Skip this entry on error rather than failing entire operation
				continue
			}
			entryHandle, _ = metadata.EncodeFileHandle(lookupFile)
		}

		// Get attributes for the entry
		// TODO: Use entry.Attr if populated to avoid this GetFile() call
		entryFile, err := metadataStore.GetFile(ctx.Context, entryHandle)
		if err != nil {
			logger.WarnCtx(ctx.Context, "READDIRPLUS: failed to get attributes", "name", entry.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "entry_handle", fmt.Sprintf("%x", entryHandle), "error", err)
			// Skip this entry on error - file may have been deleted during iteration
			continue
		}

		// Convert attributes to NFS format
		nfsEntryAttr := h.convertFileAttrToNFS(entryHandle, &entryFile.FileAttr)

		// Create directory entry with absolute cookie position
		// Cookie = startOffset + i + 1 (absolute position in directory)
		absoluteCookie := startOffset + uint64(i+1)

		plusEntry := &DirPlusEntry{
			Fileid:     entry.ID,
			Name:       entry.Name,
			Cookie:     absoluteCookie,
			Attr:       nfsEntryAttr,
			FileHandle: []byte(entryHandle),
		}

		entries = append(entries, plusEntry)

		logger.DebugCtx(ctx.Context, "READDIRPLUS: added entry", "name", entry.Name, "cookie", absoluteCookie, "fileid", entry.ID)
	}

	// ========================================================================
	// Step 7: Build success response
	// ========================================================================

	// EOF is true when there are no more pages
	eof := !page.HasMore

	logger.InfoCtx(ctx.Context, "READDIRPLUS successful", "handle", fmt.Sprintf("%x", req.DirHandle), "entries", len(entries), "eof", eof, "client", clientIP)

	logger.DebugCtx(ctx.Context, "READDIRPLUS details", "handle", fmt.Sprintf("%x", dirHandle), "total_entries", len(page.Entries), "eof", eof)

	return &ReadDirPlusResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		DirAttr:         nfsDirAttr,
		CookieVerf:      currentVerifier, // RFC 1813: verifier based on directory mtime
		Entries:         entries,
		Eof:             eof,
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
