package handlers

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// directoryMtimeVerifier generates a cookie verifier from directory mtime.
// Using mtime ensures the verifier changes when directory contents change.
// This is a common and efficient implementation per RFC 1813 recommendations.
func directoryMtimeVerifier(mtime time.Time) uint64 {
	return uint64(mtime.UnixNano())
}

// ============================================================================
// Request and Response Structures
// ============================================================================

// ReadDirRequest represents a READDIR request from an NFS client.
// The client requests a list of directory entries, optionally resuming
// from a previous position using a cookie.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.16 specifies the READDIR procedure as:
//
//	READDIR3res NFSPROC3_READDIR(READDIR3args) = 16;
//
// READDIR is used to list directory contents. Unlike READDIRPLUS, it only
// returns filenames and file IDs without attributes, making it more efficient
// for simple directory listings.
type ReadDirRequest struct {
	// DirHandle is the file handle of the directory to read.
	// Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Cookie is an opaque value used to resume reading from a specific position.
	// Set to 0 to start reading from the beginning.
	// Non-zero values should be obtained from previous READDIR responses.
	Cookie uint64

	// CookieVerf is a cookie verifier to ensure consistency across calls.
	// The server returns this value in responses, and the client should
	// pass it back in subsequent requests. A value of 0 indicates the first request.
	// If the directory changes, the server may invalidate the cookie.
	CookieVerf uint64

	// Count is the maximum number of bytes the client is willing to receive.
	// The server will attempt to fit as many entries as possible within this limit.
	// Typical values: 4096-8192 bytes.
	Count uint32
}

// ReadDirResponse represents the response to a READDIR request.
// It contains the directory's post-operation attributes, a list of entries,
// and an EOF flag indicating if all entries have been returned.
//
// The response is encoded in XDR format before being sent back to the client.
type ReadDirResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// DirAttr contains post-operation attributes of the directory.
	// Optional, may be nil. Helps clients maintain cache consistency.
	DirAttr *types.NFSFileAttr

	// CookieVerf is the cookie verifier for this directory.
	// Clients should pass this value back in subsequent READDIR requests.
	// If the directory is modified, the server may change this value to
	// invalidate outstanding cookies.
	CookieVerf uint64

	// Entries is the list of directory entries returned.
	// May be empty if the directory is empty or if starting from a cookie
	// that points beyond the last entry.
	Entries []*types.DirEntry

	// Eof indicates whether this is the last batch of entries.
	// true: All entries have been returned, no more calls needed
	// false: More entries available, client should call again with updated cookie
	Eof bool
}

// ============================================================================
// Protocol Handler
// ============================================================================

// ReadDir reads directory entries from a directory.
//
// This implements the NFS READDIR procedure as defined in RFC 1813 Section 3.3.16.
//
// **Purpose:**
//
// READDIR is used to list directory contents efficiently. It returns only:
//   - File IDs (for identifying entries)
//   - Filenames (for display)
//   - Cookies (for pagination)
//
// This is more efficient than READDIRPLUS when attributes aren't needed.
// Common use cases:
//   - Simple directory listings (ls command)
//   - File browsers without detailed information
//   - Searching for specific filenames
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format, count limits)
//  3. Extract client IP and authentication credentials from context
//  4. Verify directory handle exists and is a directory (via store)
//  5. Check for cancellation before expensive ReadDir operation
//  6. Delegate entry listing to store.ReadDir()
//  7. Return entries with proper pagination support
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after directory lookup, before ReadDir (most important check)
//   - Check after ReadDir for cancellation during the listing operation
//   - Returns NFS3ErrIO status with context error for cancellation
//   - store's ReadDir should also respect context internally for long scans
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (entry listing, ".", "..", access control) delegated to store
//   - File handle validation performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Pagination:**
//
// READDIR supports pagination for large directories:
//  1. Client calls with cookie=0 to start
//  2. Server returns entries and a cookie for the last entry
//  3. Client calls again with that cookie to continue
//  4. Server returns more entries starting after the cookie
//  5. When Eof=true, all entries have been returned
//
// **Cookie Semantics:**
//
//   - Cookie 0: Start of directory
//   - Cookie 1: After "." entry
//   - Cookie 2: After ".." entry
//   - Cookie 3+: After regular entries
//   - Cookies are opaque to clients and may be invalidated if directory changes
//
// **Cookie Verifier:**
//
// NOTE: The NFS protocol supports cookie verifiers to help detect directory modifications.
// In this implementation, cookie verification is NOT supported: the server always returns
// a cookie verifier value of 0 and does not check for directory changes between calls.
// If directory modification detection is required, this must be implemented in future versions.
// Clients should not rely on cookie verification in this implementation.
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the store, which can implement:
//   - Execute permission checking on the directory (search permission)
//   - Access control based on UID/GID
//   - Filtering entries based on permissions
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes:
//   - Directory not found → types.NFS3ErrNoEnt
//   - Not a directory → types.NFS3ErrNotDir
//   - Invalid cookie → NFS3ErrBadCookie
//   - Access denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Performance Considerations:**
//
//   - Count parameter allows clients to control response size
//   - Server should honor Count but may return fewer entries
//   - Large directories require multiple calls (pagination)
//   - store should optimize entry iteration
//   - Context cancellation prevents wasted work on large directories
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store enforces directory search permission
//   - Cookie validation prevents directory traversal attacks
//   - Client context enables audit logging
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - metadataStore: The metadata store for directory operations
//   - req: The readdir request containing directory handle and pagination info
//
// **Returns:**
//   - *ReadDirResponse: Response with status and directory entries (if successful)
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.16: READDIR Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &ReadDirRequest{
//	    DirHandle:  dirHandle,
//	    Cookie:     0, // Start from beginning
//	    CookieVerf: 0,
//	    Count:      4096,
//	}
//	ctx := &ReadDirContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.ReadDir(ctx, store, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    for _, entry := range resp.Entries {
//	        fmt.Printf("%s (id=%d)\n", entry.Name, entry.Fileid)
//	    }
//	    if !resp.Eof {
//	        // More entries available, call again with last cookie
//	    }
//	}
func (h *Handler) ReadDir(
	ctx *NFSHandlerContext,
	req *ReadDirRequest,
) (*ReadDirResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "READDIR cancelled before processing", "handle", fmt.Sprintf("%x", req.DirHandle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &ReadDirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "READDIR", "handle", fmt.Sprintf("%x", req.DirHandle), "cookie", req.Cookie, "count", req.Count, "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateReadDirRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "READDIR validation failed", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", err)
		return &ReadDirResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata store from context
	// ========================================================================

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "READDIR failed: metadata service not initialized", "client", clientIP, "error", err)
		return &ReadDirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	dirHandle := metadata.FileHandle(req.DirHandle)

	logger.DebugCtx(ctx.Context, "READDIR", "share", ctx.Share)

	// ========================================================================
	// Step 3: Verify directory handle exists and is valid
	// ========================================================================

	dirFile, status, err := h.getFileOrError(ctx, dirHandle, "READDIR", req.DirHandle)
	if dirFile == nil {
		return &ReadDirResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Verify it's actually a directory
	if dirFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "READDIR failed: handle not a directory", "handle", fmt.Sprintf("%x", req.DirHandle), "type", dirFile.Type, "client", clientIP)

		// Include directory attributes even on error for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &ReadDirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Cookie Verifier Validation (RFC 1813 Section 3.3.16)
	// ========================================================================
	// Generate verifier from directory mtime - changes when directory is modified
	currentVerifier := directoryMtimeVerifier(dirFile.Mtime)

	// Validate cookie verifier for non-initial requests
	// Initial request (cookie=0) or clients that don't use verifiers (verifier=0) bypass this check
	if req.Cookie != 0 && req.CookieVerf != 0 && req.CookieVerf != currentVerifier {
		logger.WarnCtx(ctx.Context, "READDIR: directory modified since last read",
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"expected_verf", fmt.Sprintf("0x%016x", req.CookieVerf),
			"current_verf", fmt.Sprintf("0x%016x", currentVerifier),
			"client", clientIP)

		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)
		return &ReadDirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadCookie},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 3: Build authentication context for store
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "READDIR cancelled during auth context building", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

			// Include directory attributes for cache consistency
			nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

			return &ReadDirResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				DirAttr:         nfsDirAttr,
			}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "READDIR failed: failed to build auth context", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

		// Include directory attributes for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &ReadDirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// Check for cancellation before the potentially expensive ReadDir operation
	// This is the most important check since ReadDir may scan many entries
	// in large directories
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "READDIR cancelled before reading entries", "handle", fmt.Sprintf("%x", req.DirHandle), "cookie", req.Cookie, "client", clientIP, "error", ctx.Context.Err())

		// Include directory attributes for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &ReadDirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttr:         nfsDirAttr,
		}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 4: Read directory entries via store
	// ========================================================================
	// The store is responsible for:
	// - Checking read/execute permission on the directory
	// - Building "." and ".." entries
	// - Iterating through children
	// - Handling cookie-based pagination
	// - Respecting count limits
	// - Respecting context cancellation internally during iteration

	page, err := metaSvc.ReadDirectory(authCtx, dirHandle, req.Cookie, req.Count)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "READDIR cancelled during directory scan", "handle", fmt.Sprintf("%x", req.DirHandle), "cookie", req.Cookie, "client", clientIP, "error", ctx.Context.Err())

			// Include directory attributes for cache consistency
			nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

			return &ReadDirResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				DirAttr:         nfsDirAttr,
			}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "READDIR failed: store error", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

		// Map store error to NFS status
		status := mapMetadataErrorToNFS(err)

		// Include directory attributes for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &ReadDirResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 5: Convert metadata entries to NFS wire format
	// ========================================================================
	// No cancellation check here - this is fast pure computation

	nfsEntries := make([]*types.DirEntry, 0, len(page.Entries))
	for _, entry := range page.Entries {
		nfsEntries = append(nfsEntries, &types.DirEntry{
			Fileid: entry.ID,
			Name:   entry.Name,
			Cookie: entry.Cookie, // Cookie from MetadataService
		})
	}

	// ========================================================================
	// Step 6: Build success response
	// ========================================================================

	// Generate directory attributes for response
	nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

	// EOF is true when there are no more pages
	eof := !page.HasMore

	logger.InfoCtx(ctx.Context, "READDIR successful", "handle", fmt.Sprintf("%x", req.DirHandle), "entries", len(nfsEntries), "eof", eof, "client", clientIP)

	logger.DebugCtx(ctx.Context, "READDIR details", "cookie_start", req.Cookie, "cookie_end", getLastCookie(nfsEntries), "count_limit", req.Count)

	return &ReadDirResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		DirAttr:         nfsDirAttr,
		CookieVerf:      currentVerifier, // RFC 1813: verifier based on directory mtime
		Entries:         nfsEntries,
		Eof:             eof,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// readDirValidationError represents a READDIR request validation error.
type readDirValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *readDirValidationError) Error() string {
	return e.message
}

// validateReadDirRequest validates READDIR request parameters.
//
// Checks performed:
//   - Directory handle is not empty and within limits
//   - Count is reasonable (not zero, not excessively large)
//
// Returns:
//   - nil if valid
//   - *readDirValidationError with NFS status if invalid
func validateReadDirRequest(req *ReadDirRequest) *readDirValidationError {
	// Validate directory handle
	if len(req.DirHandle) == 0 {
		return &readDirValidationError{
			message:   "empty directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.DirHandle) > 64 {
		return &readDirValidationError{
			message:   fmt.Sprintf("directory handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &readDirValidationError{
			message:   fmt.Sprintf("directory handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate count parameter
	if req.Count == 0 {
		return &readDirValidationError{
			message:   "count cannot be zero",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Sanity check: count shouldn't be excessively large (prevent DoS)
	// Most clients use 4096-8192 bytes; 1MB is a reasonable upper limit
	if req.Count > 1024*1024 {
		return &readDirValidationError{
			message:   fmt.Sprintf("count too large: %d bytes (max 1MB)", req.Count),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// Utility Functions
// ============================================================================

// getLastCookie returns the cookie of the last entry in the list.
// Returns 0 if the list is empty.
func getLastCookie(entries []*types.DirEntry) uint64 {
	if len(entries) == 0 {
		return 0
	}
	return entries[len(entries)-1].Cookie
}
