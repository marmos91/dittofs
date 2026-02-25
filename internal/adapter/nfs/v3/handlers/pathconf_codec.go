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

// DecodePathConfRequest decodes a PATHCONF request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.20 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the PATHCONF request
//
// Returns:
//   - *PathConfRequest: The decoded request containing the file handle
//   - error: Any error encountered during decoding (malformed data, invalid handle)
//
// Example:
//
//	data := []byte{...} // XDR-encoded PATHCONF request from network
//	req, err := DecodePathConfRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle in PATHCONF procedure
func DecodePathConfRequest(data []byte) (*PathConfRequest, error) {
	// Validate minimum data length for handle length field
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode file handle
	// ========================================================================

	handle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("decode handle: %w", err)
	}
	if handle == nil {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	logger.Debug("Decoded PATHCONF request", "handle_len", len(handle))

	return &PathConfRequest{Handle: handle}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the PathConfResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.20 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//     b. linkmax (4 bytes, big-endian uint32)
//     c. name_max (4 bytes, big-endian uint32)
//     d. no_trunc (4 bytes, big-endian bool as uint32)
//     e. chown_restricted (4 bytes, big-endian bool as uint32)
//     f. case_insensitive (4 bytes, big-endian bool as uint32)
//     g. case_preserving (4 bytes, big-endian bool as uint32)
//  3. If status != types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//
// XDR encoding requires all data to be in big-endian format and aligned
// to 4-byte boundaries. Boolean values are encoded as uint32 (0 or 1).
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
//
// Example:
//
//	resp := &PathConfResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    Attr:            fileAttr,
//	    Linkmax:         32767,
//	    NameMax:         255,
//	    NoTrunc:         true,
//	    ChownRestricted: true,
//	    CaseInsensitive: false,
//	    CasePreserving:  true,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *PathConfResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write post-op attributes (both success and error cases)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("encode attributes: %w", err)
	}

	// ========================================================================
	// If status is not OK, return early (no PATHCONF data on error)
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoded PATHCONF error response", "status", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Write PATHCONF properties in RFC-specified order
	// ========================================================================

	// Write linkmax
	if err := binary.Write(&buf, binary.BigEndian, resp.Linkmax); err != nil {
		return nil, fmt.Errorf("write linkmax: %w", err)
	}

	// Write name_max
	if err := binary.Write(&buf, binary.BigEndian, resp.NameMax); err != nil {
		return nil, fmt.Errorf("write name_max: %w", err)
	}

	// Write no_trunc (boolean as uint32)
	noTrunc := uint32(0)
	if resp.NoTrunc {
		noTrunc = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, noTrunc); err != nil {
		return nil, fmt.Errorf("write no_trunc: %w", err)
	}

	// Write chown_restricted (boolean as uint32)
	chownRestricted := uint32(0)
	if resp.ChownRestricted {
		chownRestricted = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, chownRestricted); err != nil {
		return nil, fmt.Errorf("write chown_restricted: %w", err)
	}

	// Write case_insensitive (boolean as uint32)
	caseInsensitive := uint32(0)
	if resp.CaseInsensitive {
		caseInsensitive = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, caseInsensitive); err != nil {
		return nil, fmt.Errorf("write case_insensitive: %w", err)
	}

	// Write case_preserving (boolean as uint32)
	casePreserving := uint32(0)
	if resp.CasePreserving {
		casePreserving = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, casePreserving); err != nil {
		return nil, fmt.Errorf("write case_preserving: %w", err)
	}

	logger.Debug("Encoded PATHCONF response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
