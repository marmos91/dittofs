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

// DecodeReadDirPlusRequest decodes a READDIRPLUS request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.17 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary
//  4. Cookie (8 bytes, big-endian uint64) - position in directory
//  5. Cookie verifier (8 bytes, big-endian uint64) - for cache consistency
//  6. Dir count (4 bytes, big-endian uint32) - max directory info bytes
//  7. Max count (4 bytes, big-endian uint32) - max total response bytes
//
// Returns:
//   - *ReadDirPlusRequest: The decoded request
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READDIRPLUS request from network
//	req, err := DecodeReadDirPlusRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req in READDIRPLUS procedure
func DecodeReadDirPlusRequest(data []byte) (*ReadDirPlusRequest, error) {
	// Validate minimum data length
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

	dirHandle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode directory handle: %w", err)
	}
	if dirHandle == nil {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// ========================================================================
	// Decode cookie (8 bytes, big-endian)
	// ========================================================================

	var cookie uint64
	if err := binary.Read(reader, binary.BigEndian, &cookie); err != nil {
		return nil, fmt.Errorf("failed to read cookie: %w", err)
	}

	// ========================================================================
	// Decode cookieverf (8 bytes, big-endian)
	// ========================================================================

	var cookieVerf uint64
	if err := binary.Read(reader, binary.BigEndian, &cookieVerf); err != nil {
		return nil, fmt.Errorf("failed to read cookieverf: %w", err)
	}

	// ========================================================================
	// Decode dircount (4 bytes, big-endian)
	// ========================================================================

	var dirCount uint32
	if err := binary.Read(reader, binary.BigEndian, &dirCount); err != nil {
		return nil, fmt.Errorf("failed to read dircount: %w", err)
	}

	// ========================================================================
	// Decode maxcount (4 bytes, big-endian)
	// ========================================================================

	var maxCount uint32
	if err := binary.Read(reader, binary.BigEndian, &maxCount); err != nil {
		return nil, fmt.Errorf("failed to read maxcount: %w", err)
	}

	logger.Debug("Decoded READDIRPLUS request", "handle_len", len(dirHandle), "cookie", cookie, "cookieverf", cookieVerf, "dircount", dirCount, "maxcount", maxCount)

	return &ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     cookie,
		CookieVerf: cookieVerf,
		DirCount:   dirCount,
		MaxCount:   maxCount,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadDirPlusResponse into XDR-encoded bytes suitable
// for transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.17 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op directory attributes (present flag + attributes if present)
//  3. If status == types.NFS3OK:
//     a. Cookie verifier (8 bytes)
//     b. Entry list:
//     - For each entry:
//     * value_follows flag (1)
//     * fileid (8 bytes)
//     * name length + name data + padding
//     * cookie (8 bytes)
//     * name_attributes (present flag + attributes if present)
//     * name_handle (present flag + handle if present)
//     - End marker: value_follows flag (0)
//     c. EOF flag (4 bytes, 0 or 1)
//  4. If status != types.NFS3OK:
//     a. No additional data beyond directory attributes
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
//	resp := &ReadDirPlusResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    DirAttr:    dirAttr,
//	    CookieVerf: 0,
//	    Entries:    entries,
//	    Eof:        true,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadDirPlusResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op directory attributes (both success and failure cases)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.DirAttr); err != nil {
		return nil, fmt.Errorf("failed to encode directory attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return early (no entries)
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READDIRPLUS error response", "status", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write cookie verifier
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.CookieVerf); err != nil {
		return nil, fmt.Errorf("failed to write cookieverf: %w", err)
	}

	// ========================================================================
	// Write directory entries
	// ========================================================================

	for _, entry := range resp.Entries {
		// value_follows = TRUE (1) - indicates an entry follows
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("failed to write value_follows flag: %w", err)
		}

		// Write fileid (8 bytes)
		if err := binary.Write(&buf, binary.BigEndian, entry.Fileid); err != nil {
			return nil, fmt.Errorf("failed to write fileid for entry '%s': %w", entry.Name, err)
		}

		// Write name (string: length + data + padding)
		if err := xdr.WriteXDRString(&buf, entry.Name); err != nil {
			return nil, fmt.Errorf("failed to write name for entry '%s': %w", entry.Name, err)
		}

		// Write cookie (8 bytes)
		if err := binary.Write(&buf, binary.BigEndian, entry.Cookie); err != nil {
			return nil, fmt.Errorf("failed to write cookie for entry '%s': %w", entry.Name, err)
		}

		// Write name_attributes (post-op attributes - optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, entry.Attr); err != nil {
			return nil, fmt.Errorf("failed to encode attributes for entry '%s': %w", entry.Name, err)
		}

		// Write name_handle (post-op file handle - optional)
		if err := xdr.EncodeOptionalOpaque(&buf, entry.FileHandle); err != nil {
			return nil, fmt.Errorf("failed to encode handle for entry '%s': %w", entry.Name, err)
		}
	}

	// ========================================================================
	// Write end-of-list marker
	// ========================================================================

	// value_follows = FALSE (0) - indicates no more entries
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("failed to write end-of-list marker: %w", err)
	}

	// ========================================================================
	// Write EOF flag
	// ========================================================================

	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("failed to write eof flag: %w", err)
	}

	logger.Debug("Encoded READDIRPLUS response", "bytes", buf.Len(), "status", resp.Status, "entries", len(resp.Entries), "eof", resp.Eof)

	return buf.Bytes(), nil
}
