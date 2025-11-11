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
	// Status indicates the result of the readdir operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): Directory not found
	//   - types.NFS3ErrNotDir (20): Handle is not a directory
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	//   - NFS3ErrBadCookie (10003): Invalid cookie
	Status uint32

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

// ReadDirContext contains the context information needed to process a READDIR request.
// This includes client identification, authentication details, and cancellation handling
// for access control.
type ReadDirContext struct {
	// Context carries cancellation signals and deadlines
	// The ReadDir handler checks this context to abort operations if the client
	// disconnects or the request times out, which is especially important for
	// large directories
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
	// Used for access control checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
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
//  4. Verify directory handle exists and is a directory (via repository)
//  5. Check for cancellation before expensive ReadDir operation
//  6. Delegate entry listing to repository.ReadDir()
//  7. Return entries with proper pagination support
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after directory lookup, before ReadDir (most important check)
//   - Check after ReadDir for cancellation during the listing operation
//   - Returns NFS3ErrIO status with context error for cancellation
//   - Repository's ReadDir should also respect context internally for long scans
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (entry listing, ".", "..", access control) delegated to repository
//   - File handle validation performed by repository.GetFile()
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
// The protocol layer passes these to the repository, which can implement:
//   - Execute permission checking on the directory (search permission)
//   - Access control based on UID/GID
//   - Filtering entries based on permissions
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
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
//   - Repository should optimize entry iteration
//   - Context cancellation prevents wasted work on large directories
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository enforces directory search permission
//   - Cookie validation prevents directory traversal attacks
//   - Client context enables audit logging
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - repository: The metadata repository for directory operations
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
//	resp, err := handler.ReadDir(ctx, repository, req)
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
func (h *DefaultNFSHandler) ReadDir(
	ctx *ReadDirContext,
	repository metadata.Repository,
	req *ReadDirRequest,
) (*ReadDirResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	select {
	case <-ctx.Context.Done():
		logger.Debug("READDIR cancelled before processing: dir=%x client=%s error=%v",
			req.DirHandle, ctx.ClientAddr, ctx.Context.Err())
		return &ReadDirResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("READDIR: dir=%x cookie=%d count=%d client=%s auth=%d",
		req.DirHandle, req.Cookie, req.Count, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateReadDirRequest(req); err != nil {
		logger.Warn("READDIR validation failed: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &ReadDirResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify directory handle exists and is valid
	// ========================================================================

	dirHandle := metadata.FileHandle(req.DirHandle)
	dirAttr, err := repository.GetFile(ctx.Context, dirHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("READDIR cancelled during directory lookup: dir=%x client=%s error=%v",
				req.DirHandle, clientIP, ctx.Context.Err())
			return &ReadDirResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Warn("READDIR failed: directory not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &ReadDirResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Verify it's actually a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("READDIR failed: handle not a directory: dir=%x type=%d client=%s",
			req.DirHandle, dirAttr.Type, clientIP)

		// Include directory attributes even on error for cache consistency
		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &ReadDirResponse{
			Status:  types.NFS3ErrNotDir,
			DirAttr: nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 3: Build authentication context for repository
	// ========================================================================

	authCtx := &metadata.AuthContext{
		AuthFlavor: ctx.AuthFlavor,
		UID:        ctx.UID,
		GID:        ctx.GID,
		GIDs:       ctx.GIDs,
		ClientAddr: clientIP,
	}

	// Check for cancellation before the potentially expensive ReadDir operation
	// This is the most important check since ReadDir may scan many entries
	// in large directories
	select {
	case <-ctx.Context.Done():
		logger.Debug("READDIR cancelled before reading entries: dir=%x cookie=%d client=%s error=%v",
			req.DirHandle, req.Cookie, clientIP, ctx.Context.Err())

		// Include directory attributes for cache consistency
		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &ReadDirResponse{
			Status:  types.NFS3ErrIO,
			DirAttr: nfsDirAttr,
		}, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 4: Read directory entries via repository
	// ========================================================================
	// The repository is responsible for:
	// - Checking read/execute permission on the directory
	// - Building "." and ".." entries
	// - Iterating through children
	// - Handling cookie-based pagination
	// - Respecting count limits
	// - Respecting context cancellation internally during iteration

	entries, eof, err := repository.ReadDir(authCtx, dirHandle, req.Cookie, req.Count)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("READDIR cancelled during directory scan: dir=%x cookie=%d client=%s error=%v",
				req.DirHandle, req.Cookie, clientIP, ctx.Context.Err())

			// Include directory attributes for cache consistency
			dirID := xdr.ExtractFileID(dirHandle)
			nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

			return &ReadDirResponse{
				Status:  types.NFS3ErrIO,
				DirAttr: nfsDirAttr,
			}, ctx.Context.Err()
		}

		logger.Error("READDIR failed: repository error: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)

		// Check for specific error types
		if exportErr, ok := err.(*metadata.ExportError); ok {
			status := mapExportErrorToNFSStatus(exportErr)

			// Include directory attributes for cache consistency
			dirID := xdr.ExtractFileID(dirHandle)
			nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

			return &ReadDirResponse{
				Status:  status,
				DirAttr: nfsDirAttr,
			}, nil
		}

		// Generic I/O error
		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &ReadDirResponse{
			Status:  types.NFS3ErrIO,
			DirAttr: nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 5: Convert metadata entries to NFS wire format
	// ========================================================================
	// No cancellation check here - this is fast pure computation

	nfsEntries := make([]*types.DirEntry, 0, len(entries))
	for _, entry := range entries {
		nfsEntries = append(nfsEntries, &types.DirEntry{
			Fileid: entry.Fileid,
			Name:   entry.Name,
			Cookie: entry.Cookie,
		})
	}

	// ========================================================================
	// Step 6: Build success response
	// ========================================================================

	// Generate directory attributes for response
	dirID := xdr.ExtractFileID(dirHandle)
	nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

	logger.Info("READDIR successful: dir=%x entries=%d eof=%v client=%s",
		req.DirHandle, len(nfsEntries), eof, clientIP)

	logger.Debug("READDIR details: cookie_start=%d cookie_end=%d count_limit=%d",
		req.Cookie, getLastCookie(nfsEntries), req.Count)

	return &ReadDirResponse{
		Status:     types.NFS3OK,
		DirAttr:    nfsDirAttr,
		CookieVerf: 0, // Simple implementation: no verifier checking
		Entries:    nfsEntries,
		Eof:        eof,
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
// XDR Decoding
// ============================================================================

// DecodeReadDirRequest decodes a READDIR request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.16 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Cookie (8 bytes, big-endian uint64)
//  5. Cookie verifier (8 bytes, big-endian uint64)
//  6. Count (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the READDIR request
//
// Returns:
//   - *ReadDirRequest: The decoded request
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READDIR request from network
//	req, err := DecodeReadDirRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle, req.Cookie, req.Count in READDIR procedure
func DecodeReadDirRequest(data []byte) (*ReadDirRequest, error) {
	// Validate minimum data length for basic structure
	// Need at least: 4 (handle len) + 8 (cookie) + 8 (verifier) + 4 (count) = 24 bytes
	if len(data) < 24 {
		return nil, fmt.Errorf("data too short: need at least 24 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

	// Read directory handle length
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	// Validate handle length
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Read directory handle
	dirHandle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &dirHandle); err != nil {
		return nil, fmt.Errorf("read handle: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for range padding {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("read padding: %w", err)
		}
	}

	// ========================================================================
	// Decode cookie
	// ========================================================================

	var cookie uint64
	if err := binary.Read(reader, binary.BigEndian, &cookie); err != nil {
		return nil, fmt.Errorf("read cookie: %w", err)
	}

	// ========================================================================
	// Decode cookie verifier
	// ========================================================================

	var cookieVerf uint64
	if err := binary.Read(reader, binary.BigEndian, &cookieVerf); err != nil {
		return nil, fmt.Errorf("read cookieverf: %w", err)
	}

	// ========================================================================
	// Decode count
	// ========================================================================

	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read count: %w", err)
	}

	logger.Debug("Decoded READDIR request: handle_len=%d cookie=%d count=%d",
		handleLen, cookie, count)

	return &ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     cookie,
		CookieVerf: cookieVerf,
		Count:      count,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadDirResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.16 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op directory attributes (present flag + attributes if present)
//  3. If status == types.NFS3OK:
//     a. Cookie verifier (8 bytes, big-endian uint64)
//     b. Directory entries (variable length list):
//     - For each entry:
//     * value_follows flag (4 bytes, 1=more entries)
//     * File ID (8 bytes, big-endian uint64)
//     * Filename (length + data + padding)
//     * Cookie (8 bytes, big-endian uint64)
//     - End marker: value_follows=0
//     c. EOF flag (4 bytes, 1=no more entries, 0=more available)
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
//	resp := &ReadDirResponse{
//	    Status:     types.NFS3OK,
//	    DirAttr:    dirAttr,
//	    CookieVerf: 0,
//	    Entries: []*DirEntry{
//	        {Fileid: 2, Name: ".", Cookie: 1},
//	        {Fileid: 1, Name: "..", Cookie: 2},
//	        {Fileid: 100, Name: "file.txt", Cookie: 3},
//	    },
//	    Eof: true,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadDirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write post-op directory attributes (always, even on error)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.DirAttr); err != nil {
		return nil, fmt.Errorf("encode directory attributes: %w", err)
	}

	// ========================================================================
	// If status is not OK, return early (no entries on error)
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READDIR error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Write cookie verifier
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.CookieVerf); err != nil {
		return nil, fmt.Errorf("write cookieverf: %w", err)
	}

	// ========================================================================
	// Write directory entries
	// ========================================================================

	for _, entry := range resp.Entries {
		// value_follows = TRUE (1) - indicates another entry follows
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("write value_follows: %w", err)
		}

		// Write file ID (8 bytes)
		if err := binary.Write(&buf, binary.BigEndian, entry.Fileid); err != nil {
			return nil, fmt.Errorf("write fileid: %w", err)
		}

		// Write filename (length + data + padding)
		nameLen := uint32(len(entry.Name))
		if err := binary.Write(&buf, binary.BigEndian, nameLen); err != nil {
			return nil, fmt.Errorf("write name length: %w", err)
		}

		if _, err := buf.Write([]byte(entry.Name)); err != nil {
			return nil, fmt.Errorf("write name: %w", err)
		}

		// Add padding to 4-byte boundary
		padding := (4 - (nameLen % 4)) % 4
		for range padding {
			if err := buf.WriteByte(0); err != nil {
				return nil, fmt.Errorf("write padding: %w", err)
			}
		}

		// Write cookie (8 bytes)
		if err := binary.Write(&buf, binary.BigEndian, entry.Cookie); err != nil {
			return nil, fmt.Errorf("write cookie: %w", err)
		}
	}

	// ========================================================================
	// Write end of entries marker
	// ========================================================================

	// value_follows = FALSE (0) - indicates no more entries in this response
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("write end marker: %w", err)
	}

	// ========================================================================
	// Write EOF flag
	// ========================================================================

	// EOF flag: 1 = all entries returned, 0 = more entries available
	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("write eof: %w", err)
	}

	logger.Debug("Encoded READDIR response: %d bytes entries=%d eof=%v",
		buf.Len(), len(resp.Entries), resp.Eof)

	return buf.Bytes(), nil
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

// mapExportErrorToNFSStatus maps metadata.ExportError to NFS status codes.
func mapExportErrorToNFSStatus(err *metadata.ExportError) uint32 {
	switch err.Code {
	case metadata.ExportErrAccessDenied:
		return types.NFS3ErrAcces
	case metadata.ExportErrNotFound:
		return types.NFS3ErrNoEnt
	case metadata.ExportErrAuthRequired:
		return types.NFS3ErrAcces
	default:
		return types.NFS3ErrIO
	}
}
