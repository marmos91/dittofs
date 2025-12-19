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

// DecodeSymlinkRequest decodes a SYMLINK request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.10 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Symlink name length (4 bytes, big-endian uint32)
//  5. Symlink name data (variable length, up to 255 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//  7. Symlink attributes (sattr3 structure)
//  8. Target path length (4 bytes, big-endian uint32)
//  9. Target path data (variable length, up to 4096 bytes)
//  10. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the SYMLINK request
//
// Returns:
//   - *SymlinkRequest: The decoded request containing directory handle, name, target, and attributes
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded SYMLINK request from network
//	req, err := DecodeSymlinkRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle, req.Name, req.Target in SYMLINK procedure
func DecodeSymlinkRequest(data []byte) (*SymlinkRequest, error) {
	// Validate minimum data length
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

	dirHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}

	// ========================================================================
	// Decode symlink name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode symlink name: %w", err)
	}

	// ========================================================================
	// Decode symlink attributes
	// ========================================================================

	attr, err := xdr.DecodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}

	// ========================================================================
	// Decode target path
	// ========================================================================

	target, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode target path: %w", err)
	}

	logger.Debug("Decoded SYMLINK request", "handle_len", len(dirHandle), "name", name, "target", target, "target_len", len(target))

	return &SymlinkRequest{
		DirHandle: dirHandle,
		Name:      name,
		Target:    target,
		Attr:      *attr,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the SymlinkResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.10 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. Post-op file handle (present flag + handle if present)
//     b. Post-op symlink attributes (present flag + attributes if present)
//  3. Directory WCC data (always present):
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
//	resp := &SymlinkResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    FileHandle:    encodedHandle,   // []byte - encoded file handle
//	    Attr:          symlinkAttr,     // *types.NFSFileAttr - NFS attributes
//	    DirAttrBefore: wccBefore,
//	    DirAttrAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *SymlinkResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Success case: Write symlink handle and attributes
	// ========================================================================

	if resp.Status == types.NFS3OK {
		// Write post-op file handle (optional)
		if err := xdr.EncodeOptionalOpaque(&buf, resp.FileHandle); err != nil {
			return nil, fmt.Errorf("encode file handle: %w", err)
		}

		// Write post-op symlink attributes (optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode symlink attributes: %w", err)
		}
	}

	// ========================================================================
	// Write directory WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.

	if err := xdr.EncodeWccData(&buf, resp.DirAttrBefore, resp.DirAttrAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded SYMLINK response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
