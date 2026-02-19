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

// DecodeSetAttrRequest decodes a SETATTR request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.2 specifications:
//  1. File handle (opaque, variable length with padding)
//  2. New attributes (sattr3 structure with optional fields)
//  3. Guard (sattrguard3 - optional time check)
//
// Returns:
//   - *SetAttrRequest: The decoded request
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded SETATTR request from network
//	req, err := DecodeSetAttrRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.NewAttr, req.Guard in SETATTR procedure
func DecodeSetAttrRequest(data []byte) (*SetAttrRequest, error) {
	// Validate minimum data length
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes, got %d", len(data))
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

	// ========================================================================
	// Decode new attributes (sattr3)
	// ========================================================================

	newAttr, err := xdr.DecodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}

	// ========================================================================
	// Decode guard (sattrguard3)
	// ========================================================================

	guard := types.TimeGuard{}

	// Read guard check flag (4 bytes, 0 or 1)
	var guardCheck uint32
	if err := binary.Read(reader, binary.BigEndian, &guardCheck); err != nil {
		return nil, fmt.Errorf("decode guard check: %w", err)
	}

	guard.Check = (guardCheck == 1)

	// If check is enabled, read the expected ctime
	if guard.Check {
		// Read seconds (4 bytes)
		if err := binary.Read(reader, binary.BigEndian, &guard.Time.Seconds); err != nil {
			return nil, fmt.Errorf("decode guard time seconds: %w", err)
		}

		// Read nanoseconds (4 bytes)
		if err := binary.Read(reader, binary.BigEndian, &guard.Time.Nseconds); err != nil {
			return nil, fmt.Errorf("decode guard time nseconds: %w", err)
		}

		logger.Debug("Decoded SETATTR guard",
			"check", true,
			"ctime", fmt.Sprintf("%d.%d", guard.Time.Seconds, guard.Time.Nseconds))
	} else {
		logger.Debug("Decoded SETATTR guard",
			"check", false)
	}

	logger.Debug("Decoded SETATTR request",
		"handle_len", len(handle),
		"guard", guard.Check)

	return &SetAttrRequest{
		Handle:  handle,
		NewAttr: *newAttr,
		Guard:   guard,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the SetAttrResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.2 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. WCC data (always present):
//     a. Pre-op attributes (present flag + attributes if present)
//     b. Post-op attributes (present flag + attributes if present)
//
// WCC data is included for both success and failure cases to help
// clients maintain cache consistency.
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
//	resp := &SetAttrResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    AttrBefore: wccBefore,
//	    AttrAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *SetAttrResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the file.

	if err := xdr.EncodeWccData(&buf, resp.AttrBefore, resp.AttrAfter); err != nil {
		return nil, fmt.Errorf("encode wcc data: %w", err)
	}

	logger.Debug("Encoded SETATTR response",
		"bytes", buf.Len(),
		"status", resp.Status)
	return buf.Bytes(), nil
}
