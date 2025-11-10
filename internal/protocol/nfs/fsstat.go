package nfs

import (
	"bytes"
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

// FsStatRequest represents an FSSTAT request from an NFS client.
// The client provides a file handle to query filesystem statistics for.
//
// RFC 1813 Section 3.3.18 specifies the FSSTAT procedure as:
//
//	FSSTAT3res NFSPROC3_FSSTAT(FSSTAT3args) = 18;
//
// The request contains only a file handle, typically the root handle of the
// mounted filesystem.
type FsStatRequest struct {
	// Handle is the file handle for which to retrieve filesystem statistics.
	// This is typically the root handle obtained from the MOUNT procedure.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// FsStatResponse represents the response to an FSSTAT request.
// It contains the status of the operation and, if successful, the current
// filesystem statistics and optional post-operation attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type FsStatResponse struct {
	// Status indicates the result of the filesystem stat operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): File handle not found
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	//   - NFS3ErrServerFault (10006): Internal server error
	Status uint32

	// Attr contains the post-operation attributes for the file handle.
	// This is optional and may be nil if Status != types.NFS3OK.
	// Including attributes helps clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Tbytes is the total size of the filesystem in bytes.
	// Only present when Status == types.NFS3OK.
	Tbytes uint64

	// Fbytes is the free space available in bytes.
	// Only present when Status == types.NFS3OK.
	Fbytes uint64

	// Abytes is the free space available to non-privileged users in bytes.
	// This may be less than Fbytes if space is reserved for root/admin.
	// Only present when Status == types.NFS3OK.
	Abytes uint64

	// Tfiles is the total number of file slots (inodes) in the filesystem.
	// Only present when Status == types.NFS3OK.
	Tfiles uint64

	// Ffiles is the number of free file slots available.
	// Only present when Status == types.NFS3OK.
	Ffiles uint64

	// Afiles is the number of file slots available to non-privileged users.
	// This may be less than Ffiles if slots are reserved for root/admin.
	// Only present when Status == types.NFS3OK.
	Afiles uint64

	// Invarsec is the number of seconds for which the filesystem is not
	// expected to change. A value of 0 means the filesystem is expected to
	// change at any time. This helps clients optimize stat operations.
	// Only present when Status == types.NFS3OK.
	Invarsec uint32
}

// FsStatContext contains the context information needed to process an FSSTAT request.
// This includes client identification and authentication details for access control
// and auditing purposes.
type FsStatContext struct {
	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client.
	// 0 = AUTH_NULL, 1 = AUTH_UNIX, etc.
	AuthFlavor uint32
}

// ============================================================================
// Handler Implementation
// ============================================================================

// FsStat handles the FSSTAT procedure, which returns dynamic information about
// the filesystem's current state, including space usage and available inodes.
//
// The FSSTAT process follows these steps:
//  1. Validate the file handle format and size
//  2. Verify the file handle exists via repository.GetFile()
//  3. Retrieve current filesystem statistics from the repository
//  4. Retrieve file attributes for cache consistency
//  5. Return comprehensive filesystem statistics to the client
//
// Design principles:
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (space calculations, limits) is delegated to repository
//   - File handle validation is performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// Security considerations:
//   - Handle validation prevents malformed requests from causing errors
//   - Repository layer enforces access control if needed
//   - Client context enables auditing and rate limiting
//   - No sensitive information leaked in error messages
//
// Per RFC 1813 Section 3.3.18:
//
//	"Procedure FSSTAT retrieves volatile information about a file system.
//	 The semantics of the size and space related fields are:
//	 - tbytes: Total size of the file system in bytes
//	 - fbytes: Free bytes in the file system
//	 - abytes: Number of free bytes available to non-privileged users"
//
// Parameters:
//   - repository: The metadata repository containing filesystem statistics
//   - req: The FSSTAT request containing the file handle
//   - ctx: Context information including client address and auth flavor
//
// Returns:
//   - *FsStatResponse: The response with filesystem statistics (if successful)
//   - error: Returns error only for internal server failures; protocol-level
//     errors are indicated via the response Status field
//
// RFC 1813 Section 3.3.18: FSSTAT Procedure
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &FsStatRequest{Handle: rootHandle}
//	ctx := &FsStatContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	}
//	resp, err := handler.FsStat(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Use resp.Tbytes, resp.Fbytes, etc. for filesystem stats
//	}
func (h *DefaultNFSHandler) FsStat(repository metadata.Repository, req *FsStatRequest, ctx *FsStatContext) (*FsStatResponse, error) {
	logger.Debug("FSSTAT request: handle=%x client=%s", req.Handle, ctx.ClientAddr)

	// Validate file handle
	if len(req.Handle) == 0 {
		logger.Warn("FSSTAT failed: empty file handle from client=%s", ctx.ClientAddr)
		return &FsStatResponse{Status: types.NFS3ErrBadHandle}, nil
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		logger.Warn("FSSTAT failed: oversized handle (%d bytes) from client=%s", len(req.Handle), ctx.ClientAddr)
		return &FsStatResponse{Status: types.NFS3ErrBadHandle}, nil
	}

	// Validate handle length for file ID extraction (need at least 8 bytes)
	if len(req.Handle) < 8 {
		logger.Warn("FSSTAT failed: undersized handle (%d bytes) from client=%s", len(req.Handle), ctx.ClientAddr)
		return &FsStatResponse{Status: types.NFS3ErrBadHandle}, nil
	}

	// Verify the file handle exists and is valid
	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Debug("FSSTAT failed: handle not found: %v client=%s", err, ctx.ClientAddr)
		return &FsStatResponse{Status: types.NFS3ErrStale}, nil
	}

	// Retrieve filesystem statistics from the repository
	fsStats, err := repository.GetFSStats(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Error("FSSTAT failed: error retrieving statistics: %v client=%s", err, ctx.ClientAddr)
		return &FsStatResponse{Status: types.NFS3ErrIO}, nil
	}

	// Generate file ID from handle for attributes
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	logger.Info("FSSTAT successful: client=%s total=%d free=%d avail=%d tfiles=%d ffiles=%d afiles=%d",
		ctx.ClientAddr, fsStats.TotalBytes, fsStats.FreeBytes, fsStats.AvailBytes,
		fsStats.TotalFiles, fsStats.FreeFiles, fsStats.AvailFiles)

	// Build response with data from repository
	return &FsStatResponse{
		Status:   types.NFS3OK,
		Attr:     nfsAttr,
		Tbytes:   fsStats.TotalBytes,
		Fbytes:   fsStats.FreeBytes,
		Abytes:   fsStats.AvailBytes,
		Tfiles:   fsStats.TotalFiles,
		Ffiles:   fsStats.FreeFiles,
		Afiles:   fsStats.AvailFiles,
		Invarsec: fsStats.Invarsec,
	}, nil
}

// ============================================================================
// XDR Encoding and Decoding
// ============================================================================

// DecodeFsStatRequest decodes an FSSTAT request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.18 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: The XDR-encoded request bytes
//
// Returns:
//   - *FsStatRequest: The decoded request
//   - error: Returns error if decoding fails due to malformed data
//
// Errors returned:
//   - "data too short": Input buffer is too small for basic structure
//   - "read handle length": Failed to read the handle length field
//   - "invalid handle length": Handle length exceeds RFC 1813 limit (64 bytes)
//   - "read handle": Failed to read the handle data
func DecodeFsStatRequest(data []byte) (*FsStatRequest, error) {
	// Minimum size: 4 bytes for handle length
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short for FSSTAT request: got %d bytes, need at least 4", len(data))
	}

	reader := bytes.NewReader(data)

	// Read handle length
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	// Validate handle length per RFC 1813 (max 64 bytes)
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d bytes (max 64)", handleLen)
	}

	// Ensure buffer has enough data for the handle
	if len(data) < 4+int(handleLen) {
		return nil, fmt.Errorf("data too short for handle: need %d bytes, got %d", 4+int(handleLen), len(data))
	}

	// Read file handle
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("read handle: %w", err)
	}

	logger.Debug("Decoded FSSTAT request: handle_len=%d", handleLen)
	return &FsStatRequest{Handle: handle}, nil
}

// Encode serializes an FSSTAT response to XDR-encoded bytes.
//
// The encoding follows RFC 1813 Section 3.3.18 specifications:
//  1. Status (4 bytes, big-endian uint32)
//  2. Post-op attributes (optional, present flag + attributes if Status == types.NFS3OK)
//  3. Filesystem statistics (only if Status == types.NFS3OK):
//     - Total bytes (8 bytes)
//     - Free bytes (8 bytes)
//     - Available bytes (8 bytes)
//     - Total files (8 bytes)
//     - Free files (8 bytes)
//     - Available files (8 bytes)
//     - Invariant time (4 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Returns:
//   - []byte: The XDR-encoded response
//   - error: Returns error if encoding fails (typically due to I/O issues)
//
// Errors returned:
//   - "write status": Failed to write the status field
//   - "write *": Failed to write various fields
//   - "encode file attributes": Failed to encode the metadata.FileAttr structure
func (resp *FsStatResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, return early with just the status
	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding FSSTAT error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// Write post-op attributes (present flag + attributes)
	if resp.Attr != nil {
		// Attributes present (1)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("write attr present flag: %w", err)
		}
		if err := xdr.EncodeFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode file attributes: %w", err)
		}
	} else {
		// Attributes not present (0)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("write attr absent flag: %w", err)
		}
	}

	// Write filesystem statistics
	if err := binary.Write(&buf, binary.BigEndian, resp.Tbytes); err != nil {
		return nil, fmt.Errorf("write total bytes: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Fbytes); err != nil {
		return nil, fmt.Errorf("write free bytes: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Abytes); err != nil {
		return nil, fmt.Errorf("write available bytes: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Tfiles); err != nil {
		return nil, fmt.Errorf("write total files: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Ffiles); err != nil {
		return nil, fmt.Errorf("write free files: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Afiles); err != nil {
		return nil, fmt.Errorf("write available files: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Invarsec); err != nil {
		return nil, fmt.Errorf("write invarsec: %w", err)
	}

	logger.Debug("Encoded FSSTAT response: %d bytes", buf.Len())
	return buf.Bytes(), nil
}
