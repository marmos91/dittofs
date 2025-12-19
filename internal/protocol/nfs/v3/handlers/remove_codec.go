package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeRemoveRequest decodes a REMOVE request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.12 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Filename length (4 bytes, big-endian uint32)
//  5. Filename data (variable length, up to 255 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the REMOVE request
//
// Returns:
//   - *RemoveRequest: The decoded request containing directory handle and filename
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded REMOVE request from network
//	req, err := DecodeRemoveRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle and req.Filename in REMOVE procedure
func DecodeRemoveRequest(data []byte) (*RemoveRequest, error) {
	// Validate minimum data length
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

	// Read directory handle length (4 bytes, big-endian)
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

	// Read directory handle
	dirHandle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &dirHandle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read handle padding byte %d: %w", i, err)
		}
	}

	// ========================================================================
	// Decode filename
	// ========================================================================

	// Read filename length (4 bytes, big-endian)
	var filenameLen uint32
	if err := binary.Read(reader, binary.BigEndian, &filenameLen); err != nil {
		return nil, fmt.Errorf("failed to read filename length: %w", err)
	}

	// Validate filename length (NFS limit is typically 255 bytes)
	if filenameLen > 255 {
		return nil, fmt.Errorf("invalid filename length: %d (max 255)", filenameLen)
	}

	// Prevent zero-length filenames
	if filenameLen == 0 {
		return nil, fmt.Errorf("invalid filename length: 0 (must be > 0)")
	}

	// Read filename
	filenameBytes := make([]byte, filenameLen)
	if err := binary.Read(reader, binary.BigEndian, &filenameBytes); err != nil {
		return nil, fmt.Errorf("failed to read filename data: %w", err)
	}

	logger.Debug("Decoded REMOVE request", "handle_len", handleLen, "name", string(filenameBytes))

	return &RemoveRequest{
		DirHandle: dirHandle,
		Filename:  string(filenameBytes),
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the RemoveResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.12 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Directory WCC data (always present):
//     a. Pre-op attributes (present flag + attributes if present)
//     b. Post-op attributes (present flag + attributes if present)
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
//	resp := &RemoveResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    DirWccBefore: wccBefore,
//	    DirWccAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *RemoveResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write directory WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.

	if err := xdr.EncodeWccData(&buf, resp.DirWccBefore, resp.DirWccAfter); err != nil {
		return nil, fmt.Errorf("failed to encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded REMOVE response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
