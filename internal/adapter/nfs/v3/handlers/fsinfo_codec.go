package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
)

// ============================================================================
// XDR Decoding
// ============================================================================

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
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	handle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode file handle: %w", err)
	}
	if handle == nil {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	return &FsInfoRequest{Handle: handle}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

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
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
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
