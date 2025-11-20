package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
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
	// Status indicates the result of the fsinfo operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): File handle not found
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Malformed file handle
	//   - types.NFS3ErrIO (5): I/O error
	Status uint32

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

// FsInfoContext contains the context information needed to process an FSINFO request.
// This includes client identification and authentication details.
type FsInfoContext struct {
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client
	// This field is available for future authentication enhancements.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix-style authentication)
	// Currently not enforced for FSINFO as it's typically an unauthenticated operation.
	AuthFlavor uint32
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
	ctx *FsInfoContext,
	req *FsInfoRequest,
) (*FsInfoResponse, error) {
	logger.Debug("FSINFO request: handle=%x client=%s auth=%d",
		req.Handle, ctx.ClientAddr, ctx.AuthFlavor)

	// Check for context cancellation before starting any work
	// FSINFO is lightweight, but we respect cancellation to prevent
	// wasting resources on abandoned requests
	select {
	case <-ctx.Context.Done():
		logger.Debug("FSINFO cancelled: handle=%x client=%s error=%v",
			req.Handle, ctx.ClientAddr, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	// Validate file handle before using it
	if err := validateFileHandle(req.Handle); err != nil {
		logger.Debug("FSINFO failed: invalid handle: %v", err)
		return &FsInfoResponse{Status: types.NFS3ErrBadHandle}, nil
	}

	// ========================================================================
	// Decode share name from file handle
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)
	shareName, path, err := metadata.DecodeShareHandle(fileHandle)
	if err != nil {
		logger.Warn("FSINFO failed: invalid file handle: handle=%x client=%s error=%v",
			req.Handle, xdr.ExtractClientIP(ctx.ClientAddr), err)
		return &FsInfoResponse{Status: types.NFS3ErrBadHandle}, nil
	}

	// Check if share exists
	if !h.Registry.ShareExists(shareName) {
		logger.Warn("FSINFO failed: share not found: share=%s handle=%x client=%s",
			shareName, req.Handle, xdr.ExtractClientIP(ctx.ClientAddr))
		return &FsInfoResponse{Status: types.NFS3ErrStale}, nil
	}

	// Get metadata store for this share
	metadataStore, err := h.Registry.GetMetadataStoreForShare(shareName)
	if err != nil {
		logger.Error("FSINFO failed: cannot get metadata store: share=%s handle=%x client=%s error=%v",
			shareName, req.Handle, xdr.ExtractClientIP(ctx.ClientAddr), err)
		return &FsInfoResponse{Status: types.NFS3ErrIO}, nil
	}

	logger.Debug("FSINFO: share=%s path=%s", shareName, path)

	// Check for cancellation before store call
	// store operations might involve I/O or locks
	select {
	case <-ctx.Context.Done():
		logger.Debug("FSINFO cancelled before GetFile: handle=%x client=%s error=%v",
			req.Handle, ctx.ClientAddr, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	// Verify the file handle exists and is valid in the store
	// The store is responsible for validating handle format and existence
	attr, err := metadataStore.GetFile(ctx.Context, fileHandle)
	if err != nil {
		logger.Debug("FSINFO failed: handle=%x client=%s error=%v",
			req.Handle, ctx.ClientAddr, err)
		return &FsInfoResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Retrieve filesystem capabilities from the store
	// All business logic about filesystem limits is handled by the store
	// Note: We don't check cancellation here since GetFilesystemCapabilities is typically
	// a fast in-memory operation returning static configuration
	capabilities, err := metadataStore.GetFilesystemCapabilities(ctx.Context, metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Error("FSINFO failed: handle=%x client=%s error=failed to retrieve capabilities: %v",
			req.Handle, ctx.ClientAddr, err)
		return &FsInfoResponse{Status: types.NFS3ErrIO}, nil
	}

	// Defensive check: ensure store returned valid capabilities
	if capabilities == nil {
		logger.Error("FSINFO failed: handle=%x client=%s error=store returned nil capabilities",
			req.Handle, ctx.ClientAddr)
		return &FsInfoResponse{Status: types.NFS3ErrIO}, nil
	}

	// Generate file ID from handle for attributes
	// This is a protocol-layer concern for creating the NFS attribute structure
	fileid, err := ExtractFileIDFromHandle(req.Handle)
	if err != nil {
		logger.Error("FSINFO failed: handle=%x client=%s error=failed to extract file ID: %v",
			req.Handle, ctx.ClientAddr, err)
		return &FsInfoResponse{Status: types.NFS3ErrBadHandle}, nil
	}

	// Convert metadata attributes to NFS wire format
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	// Convert timestamp resolution to NFS TimeVal format
	timeDelta := durationToTimeVal(capabilities.TimestampResolution)

	// Build NFS properties bitmask from capabilities
	properties := buildNFSProperties(capabilities)

	logger.Info("FSINFO successful: client=%s rtmax=%d wtmax=%d maxfilesize=%d properties=0x%x",
		ctx.ClientAddr, capabilities.MaxReadSize, capabilities.MaxWriteSize,
		capabilities.MaxFileSize, properties)

	// Build response with data from store
	// Map the standardized FilesystemCapabilities to NFS wire format
	return &FsInfoResponse{
		Status:      types.NFS3OK,
		Attr:        nfsAttr,
		Rtmax:       capabilities.MaxReadSize,
		Rtpref:      capabilities.PreferredReadSize,
		Rtmult:      4096, // Common block size multiple
		Wtmax:       capabilities.MaxWriteSize,
		Wtpref:      capabilities.PreferredWriteSize,
		Wtmult:      4096, // Common block size multiple
		Dtpref:      8192, // Reasonable default for directory reads
		Maxfilesize: capabilities.MaxFileSize,
		TimeDelta:   timeDelta,
		Properties:  properties,
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

// DecodeFsInfoRequest decodes an FSINFO request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.19 specifications:
//  1. File handle length (4 bytes)
//  2. File handle data (variable length, up to 64 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the FSINFO request
//
// Returns:
//   - *FsInfoRequest: The decoded request containing the file handle
//   - error: Any error encountered during decoding (malformed data, invalid handle)
//
// Example:
//
//	data := []byte{...} // XDR-encoded FSINFO request from network
//	req, err := DecodeFsInfoRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle in FSINFO procedure
func DecodeFsInfoRequest(data []byte) (*FsInfoRequest, error) {
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

	return &FsInfoRequest{Handle: handle}, nil
}

// Encode serializes the FsInfoResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.19 specifications:
//  1. Status code (4 bytes)
//  2. If status == types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//     b. rtmax (4 bytes)
//     c. rtpref (4 bytes)
//     d. rtmult (4 bytes)
//     e. wtmax (4 bytes)
//     f. wtpref (4 bytes)
//     g. wtmult (4 bytes)
//     h. dtpref (4 bytes)
//     i. maxfilesize (8 bytes)
//     j. time_delta (8 bytes: 4 for seconds, 4 for nanoseconds)
//     k. properties (4 bytes)
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
//	resp := &FsInfoResponse{
//	    Status:      types.NFS3OK,
//	    Rtmax:       65536,
//	    Wtmax:       65536,
//	    Maxfilesize: 1<<63 - 1,
//	    Properties:  FSFLink | FSFSymlink | FSFHomogeneous | FSFCanSetTime,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *FsInfoResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code (4 bytes, big-endian)
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// If status is not OK, only return the status code
	// Per RFC 1813, error responses contain only the status
	if resp.Status != types.NFS3OK {
		return buf.Bytes(), nil
	}

	// Write post-op attributes (present flag + attributes if present)
	if resp.Attr != nil {
		// attributes_follow = TRUE (1)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("failed to write attr present flag: %w", err)
		}
		// Encode file attributes using helper function
		if err := xdr.EncodeFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("failed to encode attributes: %w", err)
		}
	} else {
		// attributes_follow = FALSE (0)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("failed to write attr absent flag: %w", err)
		}
	}

	// Write filesystem information fields in RFC-specified order
	// Using a slice of structs for cleaner error handling and maintainability
	fields := []struct {
		name  string
		value any
	}{
		{"rtmax", resp.Rtmax},
		{"rtpref", resp.Rtpref},
		{"rtmult", resp.Rtmult},
		{"wtmax", resp.Wtmax},
		{"wtpref", resp.Wtpref},
		{"wtmult", resp.Wtmult},
		{"dtpref", resp.Dtpref},
		{"maxfilesize", resp.Maxfilesize},
		{"time_delta.seconds", resp.TimeDelta.Seconds},
		{"time_delta.nseconds", resp.TimeDelta.Nseconds},
		{"properties", resp.Properties},
	}

	// Write each field in sequence
	for _, field := range fields {
		if err := binary.Write(&buf, binary.BigEndian, field.value); err != nil {
			return nil, fmt.Errorf("failed to write %s: %w", field.name, err)
		}
	}

	return buf.Bytes(), nil
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
// The file ID is derived from the first 8 bytes of the handle.
//
// This is a protocol-layer utility used to generate NFS file IDs
// from opaque file handles. The store defines the handle format,
// but the protocol layer needs to extract identifiers for wire protocol.
//
// Parameters:
//   - handle: The file handle (must be at least 8 bytes)
//
// Returns:
//   - uint64: The extracted file ID
//   - error: If the handle is too short or invalid
func ExtractFileIDFromHandle(handle []byte) (uint64, error) {
	// Validate handle length
	// This check is defensive; validateFileHandle should have already caught this
	if len(handle) < 8 {
		return 0, fmt.Errorf("handle too short for file ID extraction: %d bytes (need 8)", len(handle))
	}

	// Extract the first 8 bytes as a big-endian uint64
	// This matches the NFS convention of using the handle's prefix as an identifier
	fileid := binary.BigEndian.Uint64(handle[:8])

	return fileid, nil
}
