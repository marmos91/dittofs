package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeLookupRequest decodes a LOOKUP request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.3 specifications:
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
//   - data: XDR-encoded bytes containing the LOOKUP request
//
// Returns:
//   - *LookupRequest: The decoded request containing directory handle and filename
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded LOOKUP request from network
//	req, err := DecodeLookupRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle and req.Filename in LOOKUP procedure
func DecodeLookupRequest(data []byte) (*LookupRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// Decode directory handle
	dirHandle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode directory handle: %w", err)
	}
	if dirHandle == nil {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Decode filename
	filename, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read filename: %w", err)
	}
	if len(filename) == 0 {
		return nil, fmt.Errorf("invalid filename length: 0 (must be > 0)")
	}
	if len(filename) > 255 {
		return nil, fmt.Errorf("invalid filename length: %d (max 255)", len(filename))
	}

	logger.Debug("Decoded LOOKUP request",
		"handle_len", len(dirHandle),
		"filename", filename)

	return &LookupRequest{
		DirHandle: dirHandle,
		Filename:  filename,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the LookupResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.3 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. File handle (opaque: length + data + padding)
//     b. Object attributes (present flag + attributes if present)
//     c. Directory post-op attributes (present flag + attributes if present)
//  3. If status != types.NFS3OK:
//     a. Directory post-op attributes (present flag + attributes if present)
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
//	resp := &LookupResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    FileHandle: fileHandle,
//	    Attr:       fileAttr,
//	    DirAttr:    dirAttr,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *LookupResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Error case: Return status + optional directory attributes
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding LOOKUP error response",
			"status", resp.Status)

		// Write post-op directory attributes (optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.DirAttr); err != nil {
			return nil, fmt.Errorf("failed to encode directory attributes: %w", err)
		}

		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write file handle, file attributes, dir attributes
	// ========================================================================

	// Write file handle (opaque data: length + data + padding)
	if err := xdr.WriteXDROpaque(&buf, resp.FileHandle); err != nil {
		return nil, fmt.Errorf("failed to write handle: %w", err)
	}

	// Write object attributes (present flag + attributes if present)
	// attributes_follow = TRUE (1)
	if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
		return nil, fmt.Errorf("failed to write attr present flag: %w", err)
	}

	// Encode file attributes using helper function
	if err := xdr.EncodeFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode file attributes: %w", err)
	}

	// Write post-op directory attributes (optional)
	if err := xdr.EncodeOptionalFileAttr(&buf, resp.DirAttr); err != nil {
		return nil, fmt.Errorf("failed to encode directory attributes: %w", err)
	}

	logger.Debug("Encoded LOOKUP response",
		"bytes", buf.Len(),
		"status", resp.Status)
	return buf.Bytes(), nil
}
