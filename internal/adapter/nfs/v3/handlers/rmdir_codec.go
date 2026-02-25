package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeRmdirRequest decodes a RMDIR request from XDR-encoded bytes.
//
// The RMDIR request has the following XDR structure (RFC 1813 Section 3.3.13):
//
//	struct RMDIR3args {
//	    diropargs3   object;  // Parent dir handle + name
//	};
//
// Decoding process:
//  1. Read parent directory handle (variable length with padding)
//  2. Read directory name (variable length string with padding)
//
// XDR encoding details:
//   - All integers are 4-byte aligned (32-bit)
//   - Variable-length data (handles, strings) are length-prefixed
//   - Padding is added to maintain 4-byte alignment
//
// Parameters:
//   - data: XDR-encoded bytes containing the rmdir request
//
// Returns:
//   - *RmdirRequest: The decoded request
//   - error: Decoding error if data is malformed or incomplete
//
// Example:
//
//	data := []byte{...} // XDR-encoded RMDIR request from network
//	req, err := DecodeRmdirRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle, req.Name in RMDIR procedure
func DecodeRmdirRequest(data []byte) (*RmdirRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode parent directory handle
	// ========================================================================

	handle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}
	if handle == nil {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// ========================================================================
	// Decode directory name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory name: %w", err)
	}

	logger.Debug("Decoded RMDIR request", "handle_len", len(handle), "name", name)

	return &RmdirRequest{
		DirHandle: handle,
		Name:      name,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the RmdirResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The RMDIR response has the following XDR structure (RFC 1813 Section 3.3.13):
//
//	struct RMDIR3res {
//	    nfsstat3    status;
//	    union switch (status) {
//	    case NFS3_OK:
//	        wcc_data    dir_wcc;
//	    default:
//	        wcc_data    dir_wcc;
//	    } resfail;
//	};
//
// Encoding process:
//  1. Write status code (4 bytes)
//  2. Write WCC data for parent directory (both success and failure)
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
//	resp := &RmdirResponse{
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
//	    DirWccBefore: wccBefore,
//	    DirWccAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *RmdirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write WCC data for parent directory (both success and failure)
	// ========================================================================

	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.
	if err := xdr.EncodeWccData(&buf, resp.DirWccBefore, resp.DirWccAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded RMDIR response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
