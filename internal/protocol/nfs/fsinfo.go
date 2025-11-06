package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
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
	//   - NFS3OK (0): Success
	//   - NFS3ErrNoEnt (2): File handle not found
	//   - NFS3ErrStale (70): Stale file handle
	//   - NFS3ErrIO (5): I/O error
	Status uint32

	// Attr contains the post-operation attributes of the file system object.
	// Present when Status == NFS3OK. May be nil if attributes are unavailable.
	// These attributes help clients maintain cache consistency.
	Attr *FileAttr

	// Rtmax is the maximum size in bytes of a READ request.
	// Clients should not exceed this value. Only present when Status == NFS3OK.
	Rtmax uint32

	// Rtpref is the preferred size in bytes of a READ request.
	// Using this size typically provides optimal performance.
	// Only present when Status == NFS3OK.
	Rtpref uint32

	// Rtmult is the suggested multiple for READ request sizes.
	// READ sizes should ideally be multiples of this value for best performance.
	// Only present when Status == NFS3OK.
	Rtmult uint32

	// Wtmax is the maximum size in bytes of a WRITE request.
	// Clients should not exceed this value. Only present when Status == NFS3OK.
	Wtmax uint32

	// Wtpref is the preferred size in bytes of a WRITE request.
	// Using this size typically provides optimal performance.
	// Only present when Status == NFS3OK.
	Wtpref uint32

	// Wtmult is the suggested multiple for WRITE request sizes.
	// WRITE sizes should ideally be multiples of this value for best performance.
	// Only present when Status == NFS3OK.
	Wtmult uint32

	// Dtpref is the preferred size in bytes of a READDIR request.
	// Using this size typically provides optimal performance for directory reads.
	// Only present when Status == NFS3OK.
	Dtpref uint32

	// Maxfilesize is the maximum file size in bytes supported by the server.
	// Attempts to create or extend files beyond this size will fail.
	// Only present when Status == NFS3OK.
	Maxfilesize uint64

	// TimeDelta represents the server's time resolution (granularity).
	// This indicates the smallest time difference the server can reliably distinguish.
	// Only present when Status == NFS3OK.
	TimeDelta TimeVal

	// Properties is a bitmask of filesystem properties indicating supported features.
	// Only present when Status == NFS3OK.
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
	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string
}

// FsInfo handles the FSINFO procedure, which returns static information about
// the NFS server's capabilities and the filesystem.
//
// The FSINFO procedure provides clients with essential information for optimizing
// their operations:
//  1. Verify the file handle exists and is valid
//  2. Retrieve filesystem capabilities from the repository
//  3. Retrieve file attributes for cache consistency
//  4. Return comprehensive filesystem information
//
// Design principles:
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (filesystem limits, capabilities) is delegated to repository
//   - File handle validation is performed by repository.GetFile()
//   - Comprehensive logging at DEBUG level for troubleshooting
//
// Per RFC 1813 Section 3.3.19:
//
//	"Procedure FSINFO retrieves non-volatile information about a file system.
//	On return, obj_attributes contains the attributes for the file system
//	object specified by fsroot."
//
// Parameters:
//   - repository: The metadata repository containing filesystem configuration
//   - req: The FSINFO request containing the file handle
//   - ctx: Context information including client address and auth flavor
//
// Returns:
//   - *FsInfoResponse: The response with filesystem information (if successful)
//   - error: Returns error only for internal server failures; protocol-level
//     errors are indicated via the response Status field
//
// RFC 1813 Section 3.3.19: FSINFO Procedure
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &FsInfoRequest{Handle: rootHandle}
//	ctx := &FsInfoContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	}
//	resp, err := handler.FsInfo(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == NFS3OK {
//	    // Use resp.Rtmax, resp.Wtmax, etc. to optimize I/O
//	}
func (h *DefaultNFSHandler) FsInfo(repository metadata.Repository, req *FsInfoRequest, ctx *FsInfoContext) (*FsInfoResponse, error) {
	logger.Debug("FSINFO request: handle=%x client=%s", req.Handle, ctx.ClientAddr)

	// Verify the file handle exists and is valid
	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Debug("FSINFO failed: handle not found: %v", err)
		return &FsInfoResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Retrieve filesystem capabilities from the repository
	fsInfo, err := repository.GetFSInfo(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Error("FSINFO failed: error retrieving fsinfo: %v", err)
		return &FsInfoResponse{Status: NFS3ErrIO}, nil
	}

	// Generate file ID from handle for attributes
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := MetadataToNFSAttr(attr, fileid)

	logger.Info("FSINFO successful: client=%s rtmax=%d wtmax=%d maxfilesize=%d properties=0x%x",
		ctx.ClientAddr, fsInfo.RtMax, fsInfo.WtMax, fsInfo.MaxFileSize, fsInfo.Properties)

	// Build response with data from repository
	return &FsInfoResponse{
		Status:      NFS3OK,
		Attr:        nfsAttr,
		Rtmax:       fsInfo.RtMax,
		Rtpref:      fsInfo.RtPref,
		Rtmult:      fsInfo.RtMult,
		Wtmax:       fsInfo.WtMax,
		Wtpref:      fsInfo.WtPref,
		Wtmult:      fsInfo.WtMult,
		Dtpref:      fsInfo.DtPref,
		Maxfilesize: fsInfo.MaxFileSize,
		TimeDelta: TimeVal{
			Seconds:  fsInfo.TimeDelta.Seconds,
			Nseconds: fsInfo.TimeDelta.Nseconds,
		},
		Properties: fsInfo.Properties,
	}, nil
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
//	    // Handle decode error
//	}
//	// Use req.Handle
func DecodeFsInfoRequest(data []byte) (*FsInfoRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// Read handle length (4 bytes, big-endian)
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("failed to read handle length: %w", err)
	}

	// Validate handle length (NFS v3 handles are typically <= 64 bytes)
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	// Ensure we have enough data for the handle
	if len(data) < int(4+handleLen) {
		return nil, fmt.Errorf("data too short for handle: need %d bytes, got %d", 4+handleLen, len(data))
	}

	// Read handle data
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle: %w", err)
	}

	return &FsInfoRequest{Handle: handle}, nil
}

// Encode serializes the FsInfoResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.19 specifications:
//  1. Status code (4 bytes)
//  2. If status == NFS3OK:
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
//	    Status:      NFS3OK,
//	    Rtmax:       65536,
//	    Wtmax:       65536,
//	    Maxfilesize: 1<<63 - 1,
//	    Properties:  FSFLink | FSFSymlink | FSFHomogeneous | FSFCanSetTime,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	}
//	// Send 'data' to client
func (resp *FsInfoResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code (4 bytes)
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// If status is not OK, only return the status code
	if resp.Status != NFS3OK {
		return buf.Bytes(), nil
	}

	// Write post-op attributes (present flag + attributes if present)
	if resp.Attr != nil {
		// attributes_follow = TRUE (1)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("failed to write attr present flag: %w", err)
		}
		// Encode file attributes
		if err := encodeFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("failed to encode attributes: %w", err)
		}
	} else {
		// attributes_follow = FALSE (0)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("failed to write attr absent flag: %w", err)
		}
	}

	// Write filesystem information fields
	fields := []struct {
		name  string
		value interface{}
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

	for _, field := range fields {
		if err := binary.Write(&buf, binary.BigEndian, field.value); err != nil {
			return nil, fmt.Errorf("failed to write %s: %w", field.name, err)
		}
	}

	return buf.Bytes(), nil
}
