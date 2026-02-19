package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeGetAttrRequest decodes a GETATTR request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.1 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the GETATTR request
//
// Returns:
//   - *GetAttrRequest: The decoded request containing the file handle
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded GETATTR request from network
//	req, err := DecodeGetAttrRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle in GETATTR procedure
func DecodeGetAttrRequest(data []byte) (*GetAttrRequest, error) {
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

	logger.Debug("Decoded GETATTR request",
		"handle_len", len(handle))

	return &GetAttrRequest{Handle: handle}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the GetAttrResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.1 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. File attributes (fattr3 structure)
//  3. If status != types.NFS3OK:
//     a. No additional data
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
//	resp := &GetAttrResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    Attr:   fileAttr,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *GetAttrResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code (4 bytes, big-endian)
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// If status is not OK, return just the status (no attributes)
	// Per RFC 1813, error responses contain only the status code
	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding GETATTR error response",
			"status", resp.Status)
		return buf.Bytes(), nil
	}

	// Write file attributes using helper function
	// This encodes the complete fattr3 structure as defined in RFC 1813
	if err := xdr.EncodeFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode file attributes: %w", err)
	}

	logger.Debug("Encoded GETATTR response",
		"bytes", buf.Len())
	return buf.Bytes(), nil
}
