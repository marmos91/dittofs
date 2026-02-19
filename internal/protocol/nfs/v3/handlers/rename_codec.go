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

// DecodeRenameRequest decodes a RENAME request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.14 specifications:
//  1. Source directory handle length (4 bytes, big-endian uint32)
//  2. Source directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Source name length (4 bytes, big-endian uint32)
//  5. Source name data (variable length, up to 255 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//  7. Destination directory handle length (4 bytes, big-endian uint32)
//  8. Destination directory handle data (variable length, up to 64 bytes)
//  9. Padding to 4-byte boundary (0-3 bytes)
//  10. Destination name length (4 bytes, big-endian uint32)
//  11. Destination name data (variable length, up to 255 bytes)
//  12. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the RENAME request
//
// Returns:
//   - *RenameRequest: The decoded request
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded RENAME request from network
//	req, err := DecodeRenameRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req in RENAME procedure
func DecodeRenameRequest(data []byte) (*RenameRequest, error) {
	// Validate minimum data length
	if len(data) < 16 {
		return nil, fmt.Errorf("data too short: need at least 16 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode source directory handle
	// ========================================================================

	fromDirHandle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("decode source directory handle: %w", err)
	}
	if fromDirHandle == nil {
		return nil, fmt.Errorf("invalid source directory handle length: 0 (must be > 0)")
	}

	// ========================================================================
	// Decode source name
	// ========================================================================

	fromName, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode source name: %w", err)
	}

	// ========================================================================
	// Decode destination directory handle
	// ========================================================================

	toDirHandle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("decode destination directory handle: %w", err)
	}
	if toDirHandle == nil {
		return nil, fmt.Errorf("invalid destination directory handle length: 0 (must be > 0)")
	}

	// ========================================================================
	// Decode destination name
	// ========================================================================

	toName, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode destination name: %w", err)
	}

	logger.Debug("Decoded RENAME request", "from", fromName, "from_dir_len", len(fromDirHandle), "to", toName, "to_dir_len", len(toDirHandle))

	return &RenameRequest{
		FromDirHandle: fromDirHandle,
		FromName:      fromName,
		ToDirHandle:   toDirHandle,
		ToName:        toName,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the RenameResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.14 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Source directory WCC data (pre-op and post-op attributes)
//  3. Destination directory WCC data (pre-op and post-op attributes)
//
// WCC data is included for both success and failure cases to help
// clients maintain cache consistency for both directories.
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
//	resp := &RenameResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    FromDirWccBefore: fromWccBefore,
//	    FromDirWccAfter:  fromWccAfter,
//	    ToDirWccBefore:   toWccBefore,
//	    ToDirWccAfter:    toWccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *RenameResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write WCC data for source directory (both success and failure)
	// ========================================================================

	if err := xdr.EncodeWccData(&buf, resp.FromDirWccBefore, resp.FromDirWccAfter); err != nil {
		return nil, fmt.Errorf("encode source directory wcc data: %w", err)
	}

	// ========================================================================
	// Write WCC data for destination directory (both success and failure)
	// ========================================================================

	if err := xdr.EncodeWccData(&buf, resp.ToDirWccBefore, resp.ToDirWccAfter); err != nil {
		return nil, fmt.Errorf("encode destination directory wcc data: %w", err)
	}

	logger.Debug("Encoded RENAME response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
