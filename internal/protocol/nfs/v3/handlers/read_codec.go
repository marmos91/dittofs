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

// DecodeReadRequest decodes a READ request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.6 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the READ request
//
// Returns:
//   - *ReadRequest: The decoded request containing handle, offset, and count
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READ request from network
//	req, err := DecodeReadRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Count in READ procedure
func DecodeReadRequest(data []byte) (*ReadRequest, error) {
	// Validate minimum data length
	// 4 bytes (handle length) + 8 bytes (offset) + 4 bytes (count) = 16 bytes minimum
	if len(data) < 16 {
		return nil, fmt.Errorf("data too short: need at least 16 bytes, got %d", len(data))
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

	// Validate handle length
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// PERFORMANCE OPTIMIZATION: Use stack-allocated buffer for file handles
	// File handles are max 64 bytes per RFC 1813, so we can avoid heap allocation
	var handleBuf [64]byte
	handleSlice := handleBuf[:handleLen]
	if err := binary.Read(reader, binary.BigEndian, &handleSlice); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}
	// Make a copy to return (original stack buffer will be reused)
	handle := make([]byte, handleLen)
	copy(handle, handleSlice)

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read handle padding byte %d: %w", i, err)
		}
	}

	// ========================================================================
	// Decode offset
	// ========================================================================

	var offset uint64
	if err := binary.Read(reader, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("failed to read offset: %w", err)
	}

	// ========================================================================
	// Decode count
	// ========================================================================

	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("failed to read count: %w", err)
	}

	logger.Debug("Decoded READ request", "handle_len", handleLen, "offset", offset, "count", count)

	return &ReadRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.6 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op attributes (present flag + attributes if present)
//  3. If status == types.NFS3OK:
//     a. Count (4 bytes, big-endian uint32)
//     b. Eof flag (4 bytes, big-endian bool as uint32)
//     c. Data length (4 bytes, big-endian uint32)
//     d. Data bytes (variable length)
//     e. Padding to 4-byte boundary (0-3 bytes)
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
//	resp := &ReadResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    Attr:   fileAttr,
//	    Count:  1024,
//	    Eof:    false,
//	    Data:   dataBytes,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadResponse) Encode() ([]byte, error) {
	// PERFORMANCE OPTIMIZATION: Pre-allocate buffer with estimated size
	// to avoid multiple allocations during encoding.
	//
	// Size calculation:
	//   - Status: 4 bytes
	//   - Optional file (present): 1 byte + ~84 bytes (NFSFileAttr)
	//   - Count: 4 bytes
	//   - EOF: 4 bytes
	//   - Data length: 4 bytes
	//   - Data: resp.Count bytes
	//   - Padding: 0-3 bytes
	//   Total: ~105 + data length + padding
	estimatedSize := 110 + int(resp.Count) + 3
	buf := bytes.NewBuffer(make([]byte, 0, estimatedSize))

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op attributes (both success and error cases)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return early if status is not OK
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READ error response", "status", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write count, EOF flag, and data
	// ========================================================================

	// Write count (number of bytes read)
	if err := binary.Write(buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("failed to write count: %w", err)
	}

	// Write EOF flag (boolean as uint32: 0=false, 1=true)
	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("failed to write eof flag: %w", err)
	}

	// Write data as opaque (length + data + padding)
	if err := xdr.WriteXDROpaque(buf, resp.Data); err != nil {
		return nil, fmt.Errorf("failed to write data: %w", err)
	}

	logger.Debug("Encoded READ response", "bytes_total", buf.Len(), "data_bytes", len(resp.Data), "status", resp.Status)

	return buf.Bytes(), nil
}
