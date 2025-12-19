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

// DecodeReadDirRequest decodes a READDIR request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.16 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Cookie (8 bytes, big-endian uint64)
//  5. Cookie verifier (8 bytes, big-endian uint64)
//  6. Count (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the READDIR request
//
// Returns:
//   - *ReadDirRequest: The decoded request
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READDIR request from network
//	req, err := DecodeReadDirRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle, req.Cookie, req.Count in READDIR procedure
func DecodeReadDirRequest(data []byte) (*ReadDirRequest, error) {
	// Validate minimum data length for basic structure
	// Need at least: 4 (handle len) + 8 (cookie) + 8 (verifier) + 4 (count) = 24 bytes
	if len(data) < 24 {
		return nil, fmt.Errorf("data too short: need at least 24 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

	// Read directory handle length
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	// Validate handle length
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Read directory handle
	dirHandle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &dirHandle); err != nil {
		return nil, fmt.Errorf("read handle: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for range padding {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("read padding: %w", err)
		}
	}

	// ========================================================================
	// Decode cookie
	// ========================================================================

	var cookie uint64
	if err := binary.Read(reader, binary.BigEndian, &cookie); err != nil {
		return nil, fmt.Errorf("read cookie: %w", err)
	}

	// ========================================================================
	// Decode cookie verifier
	// ========================================================================

	var cookieVerf uint64
	if err := binary.Read(reader, binary.BigEndian, &cookieVerf); err != nil {
		return nil, fmt.Errorf("read cookieverf: %w", err)
	}

	// ========================================================================
	// Decode count
	// ========================================================================

	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read count: %w", err)
	}

	logger.Debug("Decoded READDIR request", "handle_len", handleLen, "cookie", cookie, "count", count)

	return &ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     cookie,
		CookieVerf: cookieVerf,
		Count:      count,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadDirResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.16 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op directory attributes (present flag + attributes if present)
//  3. If status == types.NFS3OK:
//     a. Cookie verifier (8 bytes, big-endian uint64)
//     b. Directory entries (variable length list):
//     - For each entry:
//     * value_follows flag (4 bytes, 1=more entries)
//     * File ID (8 bytes, big-endian uint64)
//     * Filename (length + data + padding)
//     * Cookie (8 bytes, big-endian uint64)
//     - End marker: value_follows=0
//     c. EOF flag (4 bytes, 1=no more entries, 0=more available)
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
//	resp := &ReadDirResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    DirAttr:    dirAttr,
//	    CookieVerf: 0,
//	    Entries: []*DirEntry{
//	        {Fileid: 2, Name: ".", Cookie: 1},
//	        {Fileid: 1, Name: "..", Cookie: 2},
//	        {Fileid: 100, Name: "file.txt", Cookie: 3},
//	    },
//	    Eof: true,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadDirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write post-op directory attributes (always, even on error)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.DirAttr); err != nil {
		return nil, fmt.Errorf("encode directory attributes: %w", err)
	}

	// ========================================================================
	// If status is not OK, return early (no entries on error)
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READDIR error response", "status", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Write cookie verifier
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.CookieVerf); err != nil {
		return nil, fmt.Errorf("write cookieverf: %w", err)
	}

	// ========================================================================
	// Write directory entries
	// ========================================================================

	for _, entry := range resp.Entries {
		// value_follows = TRUE (1) - indicates another entry follows
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("write value_follows: %w", err)
		}

		// Write file ID (8 bytes)
		if err := binary.Write(&buf, binary.BigEndian, entry.Fileid); err != nil {
			return nil, fmt.Errorf("write fileid: %w", err)
		}

		// Write filename (length + data + padding)
		if err := xdr.WriteXDRString(&buf, entry.Name); err != nil {
			return nil, fmt.Errorf("write name: %w", err)
		}

		// Write cookie (8 bytes)
		if err := binary.Write(&buf, binary.BigEndian, entry.Cookie); err != nil {
			return nil, fmt.Errorf("write cookie: %w", err)
		}
	}

	// ========================================================================
	// Write end of entries marker
	// ========================================================================

	// value_follows = FALSE (0) - indicates no more entries in this response
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("write end marker: %w", err)
	}

	// ========================================================================
	// Write EOF flag
	// ========================================================================

	// EOF flag: 1 = all entries returned, 0 = more entries available
	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("write eof: %w", err)
	}

	logger.Debug("Encoded READDIR response", "bytes", buf.Len(), "entries", len(resp.Entries), "eof", resp.Eof)

	return buf.Bytes(), nil
}
