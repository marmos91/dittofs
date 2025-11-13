package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// PathConfRequest represents a PATHCONF request from an NFS client.
// The client provides a file handle to query POSIX-compatible information
// about the filesystem.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.20 specifies the PATHCONF procedure as:
//
//	PATHCONF3res NFSPROC3_PATHCONF(PATHCONF3args) = 20;
//
// Where PATHCONF3args contains:
//   - object: File handle for a filesystem object
//
// The PATHCONF procedure retrieves POSIX-style filesystem information
// that may vary per file or directory. This complements FSINFO which
// returns server-wide capabilities.
type PathConfRequest struct {
	// Handle is the file handle for which to retrieve PATHCONF information.
	// Can be the handle of any file or directory within the filesystem.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// PathConfResponse represents the response to a PATHCONF request.
// It contains POSIX-compatible filesystem properties that help clients
// understand filesystem behavior and limitations.
//
// The response is encoded in XDR format before being sent back to the client.
type PathConfResponse struct {
	// Status indicates the result of the pathconf operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): File handle not found
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

	// Attr contains post-operation attributes of the file system object.
	// Present when Status == types.NFS3OK. May be nil if attributes are unavailable.
	// These attributes help clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Linkmax is the maximum number of hard links to a file.
	// Only present when Status == types.NFS3OK.
	// Typical value: 32767 or higher for modern filesystems.
	Linkmax uint32

	// NameMax is the maximum length of a filename component in bytes.
	// Only present when Status == types.NFS3OK.
	// Typical value: 255 bytes for most modern filesystems.
	NameMax uint32

	// NoTrunc indicates whether the server rejects names longer than NameMax.
	// Only present when Status == types.NFS3OK.
	//   - true: Server rejects long names (returns NFS3ErrNameTooLong)
	//   - false: Server silently truncates long names
	// Modern servers typically set this to true.
	NoTrunc bool

	// ChownRestricted indicates if chown is restricted to the superuser.
	// Only present when Status == types.NFS3OK.
	//   - true: Only root/superuser can change file ownership
	//   - false: File owner can give away ownership
	// POSIX-compliant systems typically set this to true for security.
	ChownRestricted bool

	// CaseInsensitive indicates if filename comparisons are case-insensitive.
	// Only present when Status == types.NFS3OK.
	//   - true: "File.txt" and "file.txt" refer to the same file
	//   - false: Filenames are case-sensitive (POSIX standard)
	// Unix/Linux systems typically set this to false.
	CaseInsensitive bool

	// CasePreserving indicates if the filesystem preserves filename case.
	// Only present when Status == types.NFS3OK.
	//   - true: Filenames maintain their original case
	//   - false: Case information may be lost
	// Most modern filesystems set this to true.
	CasePreserving bool
}

// PathConfContext contains the context information needed to process a PATHCONF request.
// This includes client identification and authentication details.
type PathConfContext struct {
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix-style authentication)
	// Currently not enforced for PATHCONF as it's typically an unauthenticated operation.
	AuthFlavor uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// PathConf returns POSIX information about a file system object.
//
// This implements the NFS PATHCONF procedure as defined in RFC 1813 Section 3.3.20.
//
// **Purpose:**
//
// PATHCONF provides POSIX-compatible filesystem information that helps clients:
//   - Understand filename length limits
//   - Know if operations like chown are restricted
//   - Determine case sensitivity behavior
//   - Discover hard link limitations
//
// This complements FSINFO by providing properties that may vary per file
// or filesystem, whereas FSINFO provides server-wide capabilities.
//
// **Process:**
//
//  1. Check for context cancellation before starting
//  2. Validate the file handle format and length
//  3. Verify the file handle exists via store.GetFile()
//  4. Retrieve filesystem capabilities from store
//  5. Map FilesystemCapabilities to PATHCONF response
//  6. Return comprehensive PATHCONF information with file attributes
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (filesystem properties) is delegated to store
//   - File handle validation is performed by store.GetFile()
//   - FilesystemCapabilities provides POSIX-compatible properties
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//   - Respects context cancellation for graceful shutdown and timeouts
//
// **PATHCONF vs FSINFO:**
//
// The distinction between PATHCONF and FSINFO:
//   - FSINFO: Server-wide capabilities (transfer sizes, properties)
//   - PATHCONF: Filesystem-specific POSIX properties (may vary per mount)
//
// Per RFC 1813 Section 3.3.20:
//
//	"Procedure PATHCONF retrieves the pathconf information for a file
//	 or directory. If the FSF_HOMOGENEOUS bit is set in the FSINFO
//	 response, the pathconf information will be the same for all files
//	 and directories in the exported file system."
//
// **POSIX Semantics:**
//
// The returned values match POSIX pathconf() behavior:
//   - linkmax: _PC_LINK_MAX (from FilesystemCapabilities.MaxLinks)
//   - name_max: _PC_NAME_MAX (from FilesystemCapabilities.MaxNameLength)
//   - no_trunc: _PC_NO_TRUNC (from FilesystemCapabilities.NoTrunc)
//   - chown_restricted: _PC_CHOWN_RESTRICTED (from FilesystemCapabilities.ChownRestricted)
//   - case_insensitive: (from FilesystemCapabilities.CaseInsensitive)
//   - case_preserving: (from FilesystemCapabilities.CasePreserving)
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store layer can enforce access control if needed
//   - Client context enables audit logging
//   - No sensitive information leaked in error messages
//
// **Context Cancellation:**
//
// This operation respects context cancellation:
//   - Checks at operation start before any work
//   - Checks before store calls (GetFile, GetCapabilities)
//   - Returns types.NFS3ErrIO on cancellation
//
// PATHCONF is typically called infrequently (during mount or filesystem
// discovery), so cancellation overhead is negligible.
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and auth flavor
//   - metadataStore: The metadata store containing filesystem configuration
//   - req: The PATHCONF request containing the file handle
//
// **Returns:**
//   - *PathConfResponse: The response with PATHCONF information (if successful)
//   - error: Returns error only for internal server failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.20: PATHCONF Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &PathConfRequest{Handle: fileHandle}
//	ctx := &PathConfContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	}
//	resp, err := handler.PathConf(ctx, store, req)
//	if err != nil {
//	    // Internal server error occurred
//	    return nil, err
//	}
//	if resp.Status == types.NFS3OK {
//	    // Success - use resp.Linkmax, resp.NameMax, etc.
//	}
func (h *DefaultNFSHandler) PathConf(
	ctx *PathConfContext,
	metadataStore metadata.MetadataStore,
	req *PathConfRequest,
) (*PathConfResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("PATHCONF: handle=%x client=%s auth=%d",
		req.Handle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	select {
	case <-ctx.Context.Done():
		logger.Warn("PATHCONF cancelled: handle=%x client=%s error=%v",
			req.Handle, clientIP, ctx.Context.Err())
		return &PathConfResponse{Status: types.NFS3ErrIO}, nil
	default:
	}

	// ========================================================================
	// Step 2: Validate file handle
	// ========================================================================

	if err := validatePathConfHandle(req.Handle); err != nil {
		logger.Warn("PATHCONF validation failed: client=%s error=%v",
			clientIP, err)
		return &PathConfResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 3: Verify the file handle exists and is valid in the store
	// ========================================================================

	// Check context before store call
	select {
	case <-ctx.Context.Done():
		logger.Warn("PATHCONF cancelled before GetFile: handle=%x client=%s error=%v",
			req.Handle, clientIP, ctx.Context.Err())
		return &PathConfResponse{Status: types.NFS3ErrIO}, nil
	default:
	}

	fileHandle := metadata.FileHandle(req.Handle)
	attr, err := metadataStore.GetFile(ctx.Context, fileHandle)
	if err != nil {
		logger.Warn("PATHCONF failed: handle not found: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &PathConfResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// ========================================================================
	// Step 4: Retrieve filesystem capabilities from the store
	// ========================================================================
	// FilesystemCapabilities contains POSIX-compatible properties
	// that map directly to PATHCONF response fields

	// Check context before store call
	select {
	case <-ctx.Context.Done():
		logger.Warn("PATHCONF cancelled before GetCapabilities: handle=%x client=%s error=%v",
			req.Handle, clientIP, ctx.Context.Err())
		return &PathConfResponse{Status: types.NFS3ErrIO}, nil
	default:
	}

	caps, err := metadataStore.GetFilesystemCapabilities(ctx.Context, fileHandle)
	if err != nil {
		logger.Error("PATHCONF failed: could not get filesystem capabilities: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &PathConfResponse{Status: types.NFS3ErrIO}, nil
	}

	// ========================================================================
	// Step 5: Generate file attributes for cache consistency
	// ========================================================================

	fileid := xdr.ExtractFileID(fileHandle)
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	logger.Info("PATHCONF successful: handle=%x client=%s", req.Handle, clientIP)
	logger.Debug("PATHCONF properties: linkmax=%d namemax=%d no_trunc=%v chown_restricted=%v case_insensitive=%v case_preserving=%v",
		caps.MaxHardLinkCount, caps.MaxFilenameLen, !caps.TruncatesLongNames,
		caps.ChownRestricted, !caps.CaseSensitive, caps.CasePreserving)

	// ========================================================================
	// Step 6: Build response with filesystem capabilities
	// ========================================================================
	// Map FilesystemCapabilities fields to PATHCONF response fields

	return &PathConfResponse{
		Status:          types.NFS3OK,
		Attr:            nfsAttr,
		Linkmax:         caps.MaxHardLinkCount,
		NameMax:         caps.MaxFilenameLen,
		NoTrunc:         !caps.TruncatesLongNames,
		ChownRestricted: caps.ChownRestricted,
		CaseInsensitive: !caps.CaseSensitive,
		CasePreserving:  caps.CasePreserving,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// pathConfValidationError represents a PATHCONF request validation error.
type pathConfValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *pathConfValidationError) Error() string {
	return e.message
}

// validatePathConfHandle validates a PATHCONF file handle.
//
// Checks performed:
//   - Handle is not empty
//   - Handle length is within RFC 1813 limits (max 64 bytes)
//   - Handle is long enough for file ID extraction (min 8 bytes)
//
// Returns:
//   - nil if valid
//   - *pathConfValidationError with NFS status if invalid
func validatePathConfHandle(handle []byte) *pathConfValidationError {
	if len(handle) == 0 {
		return &pathConfValidationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(handle) > 64 {
		return &pathConfValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(handle) < 8 {
		return &pathConfValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodePathConfRequest decodes a PATHCONF request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.20 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the PATHCONF request
//
// Returns:
//   - *PathConfRequest: The decoded request containing the file handle
//   - error: Any error encountered during decoding (malformed data, invalid handle)
//
// Example:
//
//	data := []byte{...} // XDR-encoded PATHCONF request from network
//	req, err := DecodePathConfRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle in PATHCONF procedure
func DecodePathConfRequest(data []byte) (*PathConfRequest, error) {
	// Validate minimum data length for handle length field
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode file handle
	// ========================================================================

	handle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode handle: %w", err)
	}

	logger.Debug("Decoded PATHCONF request: handle_len=%d", len(handle))

	return &PathConfRequest{Handle: handle}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the PathConfResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.20 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//     b. linkmax (4 bytes, big-endian uint32)
//     c. name_max (4 bytes, big-endian uint32)
//     d. no_trunc (4 bytes, big-endian bool as uint32)
//     e. chown_restricted (4 bytes, big-endian bool as uint32)
//     f. case_insensitive (4 bytes, big-endian bool as uint32)
//     g. case_preserving (4 bytes, big-endian bool as uint32)
//  3. If status != types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//
// XDR encoding requires all data to be in big-endian format and aligned
// to 4-byte boundaries. Boolean values are encoded as uint32 (0 or 1).
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
//
// Example:
//
//	resp := &PathConfResponse{
//	    Status:          types.NFS3OK,
//	    Attr:            fileAttr,
//	    Linkmax:         32767,
//	    NameMax:         255,
//	    NoTrunc:         true,
//	    ChownRestricted: true,
//	    CaseInsensitive: false,
//	    CasePreserving:  true,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *PathConfResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write post-op attributes (both success and error cases)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("encode attributes: %w", err)
	}

	// ========================================================================
	// If status is not OK, return early (no PATHCONF data on error)
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoded PATHCONF error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Write PATHCONF properties in RFC-specified order
	// ========================================================================

	// Write linkmax
	if err := binary.Write(&buf, binary.BigEndian, resp.Linkmax); err != nil {
		return nil, fmt.Errorf("write linkmax: %w", err)
	}

	// Write name_max
	if err := binary.Write(&buf, binary.BigEndian, resp.NameMax); err != nil {
		return nil, fmt.Errorf("write name_max: %w", err)
	}

	// Write no_trunc (boolean as uint32)
	noTrunc := uint32(0)
	if resp.NoTrunc {
		noTrunc = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, noTrunc); err != nil {
		return nil, fmt.Errorf("write no_trunc: %w", err)
	}

	// Write chown_restricted (boolean as uint32)
	chownRestricted := uint32(0)
	if resp.ChownRestricted {
		chownRestricted = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, chownRestricted); err != nil {
		return nil, fmt.Errorf("write chown_restricted: %w", err)
	}

	// Write case_insensitive (boolean as uint32)
	caseInsensitive := uint32(0)
	if resp.CaseInsensitive {
		caseInsensitive = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, caseInsensitive); err != nil {
		return nil, fmt.Errorf("write case_insensitive: %w", err)
	}

	// Write case_preserving (boolean as uint32)
	casePreserving := uint32(0)
	if resp.CasePreserving {
		casePreserving = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, casePreserving); err != nil {
		return nil, fmt.Errorf("write case_preserving: %w", err)
	}

	logger.Debug("Encoded PATHCONF response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}
