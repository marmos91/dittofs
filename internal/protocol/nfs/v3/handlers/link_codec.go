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

// DecodeLinkRequest decodes a LINK request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.15 specifications:
//  1. Source file handle length (4 bytes, big-endian uint32)
//  2. Source file handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Target directory handle length (4 bytes, big-endian uint32)
//  5. Target directory handle data (variable length, up to 64 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//  7. Link name length (4 bytes, big-endian uint32)
//  8. Link name data (variable length, up to 255 bytes)
//  9. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the LINK request
//
// Returns:
//   - *LinkRequest: The decoded request containing file handle, directory, and name
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded LINK request from network
//	req, err := DecodeLinkRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.FileHandle, req.DirHandle, req.Name in LINK procedure
func DecodeLinkRequest(data []byte) (*LinkRequest, error) {
	// Validate minimum data length
	if len(data) < 12 {
		return nil, fmt.Errorf("data too short: need at least 12 bytes for handles, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode source file handle
	// ========================================================================

	fileHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode file handle: %w", err)
	}

	// ========================================================================
	// Decode target directory handle
	// ========================================================================

	dirHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}

	// ========================================================================
	// Decode link name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}

	logger.Debug("Decoded LINK request", "file_handle_len", len(fileHandle), "dir_handle_len", len(dirHandle), "name", name)

	return &LinkRequest{
		FileHandle: fileHandle,
		DirHandle:  dirHandle,
		Name:       name,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the LinkResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.15 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op file attributes (present flag + attributes if present)
//  3. Directory WCC data (pre-op and post-op attributes)
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
//	resp := &LinkResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    FileAttr:     fileAttr,
//	    DirWccBefore: wccBefore,
//	    DirWccAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *LinkResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write post-op file attributes (optional)
	// ========================================================================
	// Present for both success and failure cases to help clients
	// maintain cache consistency

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.FileAttr); err != nil {
		return nil, fmt.Errorf("encode file attributes: %w", err)
	}

	// ========================================================================
	// Write directory WCC data (always present)
	// ========================================================================
	// Weak cache consistency data helps clients detect if the directory
	// changed during the operation

	if err := xdr.EncodeWccData(&buf, resp.DirWccBefore, resp.DirWccAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded LINK response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
