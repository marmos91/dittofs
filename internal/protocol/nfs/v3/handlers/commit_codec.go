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

// DecodeCommitRequest decodes a COMMIT request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.21 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the COMMIT request
//
// Returns:
//   - *CommitRequest: The decoded request containing handle, offset, and count
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded COMMIT request from network
//	req, err := DecodeCommitRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Count in COMMIT procedure
func DecodeCommitRequest(data []byte) (*CommitRequest, error) {
	// Validate minimum data length (4 bytes handle len + at least 1 byte handle + 8 bytes offset + 4 bytes count)
	if len(data) < 17 {
		return nil, fmt.Errorf("data too short: need at least 17 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode file handle
	// ========================================================================

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

	// Read handle
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			break // tolerate missing padding bytes, as in DecodeWriteRequest
		}
	}

	// ========================================================================
	// Decode offset
	// ========================================================================

	// Read offset (8 bytes, big-endian)
	var offset uint64
	if err := binary.Read(reader, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("failed to read offset: %w", err)
	}

	// ========================================================================
	// Decode count
	// ========================================================================

	// Read count (4 bytes, big-endian)
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("failed to read count: %w", err)
	}

	logger.Debug("Decoded COMMIT request", "handle_len", handleLen, "offset", offset, "count", count)

	return &CommitRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the CommitResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.21 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. File WCC data (always present):
//     a. Pre-op attributes (present flag + attributes if present)
//     b. Post-op attributes (present flag + attributes if present)
//  3. Write verifier (8 bytes, big-endian uint64) - only on success
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
//	resp := &CommitResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    AttrBefore:    wccBefore,
//	    AttrAfter:     wccAfter,
//	    WriteVerifier: 0,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *CommitResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write file WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the file.

	if err := xdr.EncodeWccData(&buf, resp.AttrBefore, resp.AttrAfter); err != nil {
		return nil, fmt.Errorf("failed to encode file wcc data: %w", err)
	}

	// ========================================================================
	// Write write verifier (only on success)
	// ========================================================================

	if resp.Status == types.NFS3OK {
		if err := binary.Write(&buf, binary.BigEndian, resp.WriteVerifier); err != nil {
			return nil, fmt.Errorf("failed to write verifier: %w", err)
		}
	}

	logger.Debug("Encoded COMMIT response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
