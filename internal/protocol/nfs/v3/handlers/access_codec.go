package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeAccessRequest decodes an ACCESS request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.4 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Access bitmap (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the ACCESS request
//
// Returns:
//   - *AccessRequest: The decoded request containing handle and access bitmap
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded ACCESS request from network
//	req, err := DecodeAccessRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle and req.Access in ACCESS procedure
func DecodeAccessRequest(data []byte) (*AccessRequest, error) {
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

	// Prevent zero-length handles
	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Ensure we have enough data for the handle
	// 4 bytes for length + handleLen bytes for data + up to 3 bytes padding + 4 bytes for access
	minRequired := 4 + handleLen
	if uint32(len(data)) < minRequired {
		return nil, fmt.Errorf("data too short for handle: need at least %d bytes, got %d", minRequired, len(data))
	}

	// Read handle data
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read padding byte %d: %w", i, err)
		}
	}

	// Read access bitmap (4 bytes, big-endian)
	var access uint32
	if err := binary.Read(reader, binary.BigEndian, &access); err != nil {
		return nil, fmt.Errorf("failed to read access bitmap: %w", err)
	}

	return &AccessRequest{
		Handle: handle,
		Access: access,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the AccessResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.4 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//     b. Access bitmap (4 bytes, granted permissions)
//  3. If status != types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//     b. No access bitmap
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
//	resp := &AccessResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    Attr:   fileAttr,
//	    Access: AccessRead | AccessLookup,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *AccessResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code (4 bytes, big-endian)
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// Write post-op attributes (present flag + attributes if present)
	// These are included for both success and failure cases to help
	// clients maintain cache consistency
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

	// If status is not OK, we're done - no access bitmap on error
	if resp.Status != types.NFS3OK {
		return buf.Bytes(), nil
	}

	// Write granted access bitmap (4 bytes, big-endian)
	// This indicates which of the requested permissions were granted
	if err := binary.Write(&buf, binary.BigEndian, resp.Access); err != nil {
		return nil, fmt.Errorf("failed to write access bitmap: %w", err)
	}

	return buf.Bytes(), nil
}
