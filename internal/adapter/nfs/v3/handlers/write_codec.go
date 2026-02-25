package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeWriteRequest decodes a WRITE request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.7 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//  6. Stable (4 bytes, big-endian uint32)
//  7. Data length (4 bytes, big-endian uint32)
//  8. Data bytes (variable length)
//  9. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the WRITE request
//
// Returns:
//   - *WriteRequest: The decoded request containing handle, offset, and data
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded WRITE request from network
//	req, err := DecodeWriteRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Data in WRITE procedure
func DecodeWriteRequest(data []byte) (*WriteRequest, error) {
	if len(data) < 24 {
		return nil, fmt.Errorf("data too short: need at least 24 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// Decode file handle
	handle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode file handle: %w", err)
	}
	if handle == nil {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Decode offset
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

	// ========================================================================
	// Decode stability level
	// ========================================================================

	var stable uint32
	if err := binary.Read(reader, binary.BigEndian, &stable); err != nil {
		return nil, fmt.Errorf("failed to read stable: %w", err)
	}

	// ========================================================================
	// Decode data
	// ========================================================================

	// Read data length
	var dataLen uint32
	if err := binary.Read(reader, binary.BigEndian, &dataLen); err != nil {
		return nil, fmt.Errorf("failed to read data length: %w", err)
	}

	// Validate data length during decoding to prevent memory exhaustion.
	// This is a hard-coded safety limit to prevent allocating excessive
	// memory before we can even validate the request. The actual validation
	// will use the store's configured maximum.
	//
	// This limit (32MB) is chosen to be:
	//   - Large enough to accommodate any reasonable store configuration
	//   - Small enough to prevent memory exhaustion attacks during XDR decoding
	//   - Higher than typical NFS write sizes (64KB-1MB)
	const maxDecodingSize = 32 * 1024 * 1024 // 32MB
	if dataLen > maxDecodingSize {
		return nil, fmt.Errorf("data length too large: %d bytes (max %d for decoding)", dataLen, maxDecodingSize)
	}

	// ZERO-COPY OPTIMIZATION: Instead of allocating a new buffer, slice the
	// original data buffer. This avoids a memory allocation and copy operation.
	// The data remains valid until the pooled buffer is returned (after handler completes).
	//
	// Calculate current offset in the original data slice
	currentPos := len(data) - reader.Len()

	// Validate we have enough data remaining
	if currentPos+int(dataLen) > len(data) {
		return nil, fmt.Errorf("insufficient data: need %d bytes, have %d", dataLen, len(data)-currentPos)
	}

	// Slice the original buffer (zero-copy)
	writeData := data[currentPos : currentPos+int(dataLen)]

	// Advance the reader position to skip the data we just sliced
	if _, err := reader.Seek(int64(dataLen), io.SeekCurrent); err != nil {
		return nil, fmt.Errorf("failed to advance reader: %w", err)
	}

	// Skip padding to 4-byte boundary (XDR alignment requirement)
	dataPadding := (4 - (dataLen % 4)) % 4
	for i := uint32(0); i < dataPadding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			// Padding is optional at end of message, don't fail
			break
		}
	}

	logger.Debug("Decoded WRITE request", "handle_len", len(handle), "offset", offset, "count", count, "stable", stable, "data_len", dataLen)

	return &WriteRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
		Stable: stable,
		Data:   writeData,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the WriteResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.7 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. WCC data (weak cache consistency):
//     a. Pre-op attributes (present flag + wcc_attr if present)
//     b. Post-op attributes (present flag + file_attr if present)
//  3. If status == NFS3OK:
//     a. Count (4 bytes, big-endian uint32)
//     b. Committed (4 bytes, big-endian uint32)
//     c. Write verifier (8 bytes, big-endian uint64)
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
//	resp := &WriteResponse{
//	    NFSResponseBase: NFSResponseBase{Status: NFS3OK},
//	    AttrBefore: wccAttr,
//	    AttrAfter:  fileAttr,
//	    Count:      1024,
//	    Committed:  FileSyncWrite,
//	    Verf:       12345,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *WriteResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write WCC data (Weak Cache Consistency)
	// ========================================================================
	// WCC data is included in both success and error cases to help
	// clients maintain cache consistency.

	// Write pre-op attributes
	if resp.AttrBefore != nil {
		// Present flag = TRUE (1)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("failed to write pre-op present flag: %w", err)
		}

		// Write WCC attributes (size, mtime, ctime)
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Size); err != nil {
			return nil, fmt.Errorf("failed to write pre-op size: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Seconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op mtime seconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Nseconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op mtime nseconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Seconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op ctime seconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Nseconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op ctime nseconds: %w", err)
		}
	} else {
		// Present flag = FALSE (0)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("failed to write pre-op absent flag: %w", err)
		}
	}

	// Write post-op attributes
	if err := xdr.EncodeOptionalFileAttr(&buf, resp.AttrAfter); err != nil {
		return nil, fmt.Errorf("failed to encode post-op attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return early if status is not OK
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding WRITE error response", "status", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write count, committed, and verifier
	// ========================================================================

	// Write count (number of bytes actually written)
	if err := binary.Write(&buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("failed to write count: %w", err)
	}

	// Write committed (stability level actually achieved)
	if err := binary.Write(&buf, binary.BigEndian, resp.Committed); err != nil {
		return nil, fmt.Errorf("failed to write committed: %w", err)
	}

	// Write verifier (8 bytes - server instance identifier)
	if err := binary.Write(&buf, binary.BigEndian, resp.Verf); err != nil {
		return nil, fmt.Errorf("failed to write verifier: %w", err)
	}

	logger.Debug("Encoded WRITE response", "bytes", buf.Len(), "status", resp.Status, "count", resp.Count, "committed", resp.Committed)

	return buf.Bytes(), nil
}
