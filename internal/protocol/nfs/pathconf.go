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
//  1. Validate the file handle format and length
//  2. Verify the file handle exists via repository.GetFile()
//  3. Retrieve PATHCONF properties from the repository
//  4. Retrieve file attributes for cache consistency
//  5. Return comprehensive PATHCONF information
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (filesystem properties) is delegated to repository
//   - File handle validation is performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
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
// The returned values should match POSIX pathconf() behavior where applicable:
//   - linkmax: _PC_LINK_MAX
//   - name_max: _PC_NAME_MAX
//   - no_trunc: _PC_NO_TRUNC
//   - chown_restricted: _PC_CHOWN_RESTRICTED
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository layer can enforce access control if needed
//   - Client context enables audit logging
//   - No sensitive information leaked in error messages
//
// **Parameters:**
//   - repository: The metadata repository containing filesystem configuration
//   - req: The PATHCONF request containing the file handle
//   - ctx: Context information including client address and auth flavor
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
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	}
//	resp, err := handler.PathConf(repository, req, ctx)
//	if err != nil {
//	    // Internal server error occurred
//	    return nil, err
//	}
//	if resp.Status == types.NFS3OK {
//	    // Success - use resp.Linkmax, resp.NameMax, etc.
//	}
func (h *DefaultNFSHandler) PathConf(repository metadata.Repository, req *PathConfRequest, ctx *PathConfContext) (*PathConfResponse, error) {
	logger.Debug("PATHCONF request: handle=%x client=%s auth=%d",
		req.Handle, ctx.ClientAddr, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate file handle
	// ========================================================================

	if err := validateFileHandle(req.Handle); err != nil {
		logger.Debug("PATHCONF failed: invalid handle: %v", err)
		return &PathConfResponse{Status: types.NFS3ErrBadHandle}, nil
	}

	// ========================================================================
	// Step 2: Verify the file handle exists and is valid in the repository
	// ========================================================================

	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Debug("PATHCONF failed: handle=%x client=%s error=%v",
			req.Handle, ctx.ClientAddr, err)
		return &PathConfResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// ========================================================================
	// Step 3: Retrieve PATHCONF properties from the repository
	// ========================================================================
	// All business logic about filesystem properties is handled by the repository

	pathConf, err := repository.GetPathConf(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Error("PATHCONF failed: handle=%x client=%s error=failed to retrieve pathconf: %v",
			req.Handle, ctx.ClientAddr, err)
		return &PathConfResponse{Status: types.NFS3ErrIO}, nil
	}

	// Defensive check: ensure repository returned valid pathConf
	if pathConf == nil {
		logger.Error("PATHCONF failed: handle=%x client=%s error=repository returned nil pathconf",
			req.Handle, ctx.ClientAddr)
		return &PathConfResponse{Status: types.NFS3ErrIO}, nil
	}

	// ========================================================================
	// Step 4: Generate file attributes for cache consistency
	// ========================================================================

	fileid, err := ExtractFileIDFromHandle(req.Handle)
	if err != nil {
		logger.Error("PATHCONF failed: handle=%x client=%s error=failed to extract file ID: %v",
			req.Handle, ctx.ClientAddr, err)
		return &PathConfResponse{Status: types.NFS3ErrBadHandle}, nil
	}

	// Convert metadata attributes to NFS wire format
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	logger.Info("PATHCONF successful: client=%s handle=%x", ctx.ClientAddr, req.Handle)
	logger.Debug("PATHCONF properties: linkmax=%d namemax=%d no_trunc=%v chown_restricted=%v case_insensitive=%v case_preserving=%v",
		pathConf.Linkmax, pathConf.NameMax, pathConf.NoTrunc,
		pathConf.ChownRestricted, pathConf.CaseInsensitive, pathConf.CasePreserving)

	// ========================================================================
	// Step 5: Build response with data from repository
	// ========================================================================

	return &PathConfResponse{
		Status:          types.NFS3OK,
		Attr:            nfsAttr,
		Linkmax:         pathConf.Linkmax,
		NameMax:         pathConf.NameMax,
		NoTrunc:         pathConf.NoTrunc,
		ChownRestricted: pathConf.ChownRestricted,
		CaseInsensitive: pathConf.CaseInsensitive,
		CasePreserving:  pathConf.CasePreserving,
	}, nil
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

	// Read handle length (4 bytes, big-endian)
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("failed to read handle length: %w", err)
	}

	// Validate handle length (NFS v3 handles are typically <= 64 bytes per RFC 1813)
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	// Prevent zero-length handles which would cause issues downstream
	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Ensure we have enough data for the handle
	// 4 bytes for length + handleLen bytes for data
	if len(data) < int(4+handleLen) {
		return nil, fmt.Errorf("data too short for handle: need %d bytes total, got %d", 4+handleLen, len(data))
	}

	// Read handle data
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	logger.Debug("Decoded PATHCONF request: handle_len=%d", handleLen)

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
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op attributes (both success and error cases)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode attributes: %w", err)
	}

	// ========================================================================
	// If status is not OK, return early (no PATHCONF data on error)
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding PATHCONF error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Write PATHCONF properties in RFC-specified order
	// ========================================================================

	// Write linkmax
	if err := binary.Write(&buf, binary.BigEndian, resp.Linkmax); err != nil {
		return nil, fmt.Errorf("failed to write linkmax: %w", err)
	}

	// Write name_max
	if err := binary.Write(&buf, binary.BigEndian, resp.NameMax); err != nil {
		return nil, fmt.Errorf("failed to write name_max: %w", err)
	}

	// Write no_trunc (boolean as uint32)
	noTrunc := uint32(0)
	if resp.NoTrunc {
		noTrunc = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, noTrunc); err != nil {
		return nil, fmt.Errorf("failed to write no_trunc: %w", err)
	}

	// Write chown_restricted (boolean as uint32)
	chownRestricted := uint32(0)
	if resp.ChownRestricted {
		chownRestricted = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, chownRestricted); err != nil {
		return nil, fmt.Errorf("failed to write chown_restricted: %w", err)
	}

	// Write case_insensitive (boolean as uint32)
	caseInsensitive := uint32(0)
	if resp.CaseInsensitive {
		caseInsensitive = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, caseInsensitive); err != nil {
		return nil, fmt.Errorf("failed to write case_insensitive: %w", err)
	}

	// Write case_preserving (boolean as uint32)
	casePreserving := uint32(0)
	if resp.CasePreserving {
		casePreserving = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, casePreserving); err != nil {
		return nil, fmt.Errorf("failed to write case_preserving: %w", err)
	}

	logger.Debug("Encoded PATHCONF response: %d bytes", buf.Len())
	return buf.Bytes(), nil
}
