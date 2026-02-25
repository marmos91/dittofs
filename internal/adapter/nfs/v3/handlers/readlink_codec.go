package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeReadLinkRequest decodes a READLINK request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.5 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the READLINK request
//
// Returns:
//   - *ReadLinkRequest: The decoded request containing the symlink handle
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READLINK request from network
//	req, err := DecodeReadLinkRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle in READLINK procedure
func DecodeReadLinkRequest(data []byte) (*ReadLinkRequest, error) {
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

	logger.Debug("Decoded READLINK request", "handle_len", len(handle))

	return &ReadLinkRequest{Handle: handle}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadLinkResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.5 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. Post-op symlink attributes (present flag + attributes if present)
//     b. Target path (length + string data + padding)
//  3. If status != types.NFS3OK:
//     a. Post-op symlink attributes (present flag + attributes if present)
//     b. No target path
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
//	resp := &ReadLinkResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    Attr:   symlinkAttr,
//	    Target: "/usr/bin/python3",
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadLinkResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op symlink attributes (both success and error cases)
	// ========================================================================
	// Including attributes on error helps clients maintain cache consistency
	// for the symlink itself, even when reading the target fails

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return without target path
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READLINK error response", "status", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write target path
	// ========================================================================

	// Write target path as XDR string (length + data + padding)
	if err := xdr.WriteXDRString(&buf, resp.Target); err != nil {
		return nil, fmt.Errorf("failed to write target: %w", err)
	}

	logger.Debug("Encoded READLINK response", "bytes", buf.Len(), "target_len", len(resp.Target))
	return buf.Bytes(), nil
}
