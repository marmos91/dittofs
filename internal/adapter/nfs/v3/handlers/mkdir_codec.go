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

// DecodeMkdirRequest decodes a MKDIR request from XDR-encoded bytes.
//
// The MKDIR request has the following XDR structure (RFC 1813 Section 3.3.9):
//
//	struct MKDIR3args {
//	    diropargs3   where;     // Parent dir handle + name
//	    sattr3       attributes; // Directory attributes to set
//	};
//
// Decoding process:
//  1. Read parent directory handle (variable length with padding)
//  2. Read directory name (variable length string with padding)
//  3. Read attributes structure (sattr3)
//
// Parameters:
//   - data: XDR-encoded bytes containing the mkdir request
//
// Returns:
//   - *MkdirRequest: The decoded request
//   - error: Decoding error if data is malformed or incomplete
func DecodeMkdirRequest(data []byte) (*MkdirRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)
	req := &MkdirRequest{}

	// Decode parent directory handle
	handle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("decode handle: %w", err)
	}
	if handle == nil {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}
	req.DirHandle = handle

	// Decode directory name
	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}
	req.Name = name

	// Decode sattr3 attributes structure
	attr, err := xdr.DecodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}
	req.Attr = attr

	var mode uint32
	if attr != nil && attr.Mode != nil {
		mode = *attr.Mode
	}

	logger.Debug("Decoded MKDIR request", "handle_len", len(handle), "name", name, "mode", fmt.Sprintf("%o", mode))

	return req, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the MkdirResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.9 specifications.
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
func (resp *MkdirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Success case: Write handle and attributes
	if resp.Status == types.NFS3OK {
		// Write new directory handle (post_op_fh3 - optional)
		if err := xdr.EncodeOptionalOpaque(&buf, resp.Handle); err != nil {
			return nil, fmt.Errorf("encode handle: %w", err)
		}

		// Write new directory attributes (post_op_attr - optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode attributes: %w", err)
		}
	}

	// Write WCC data for parent directory (both success and failure)
	if err := xdr.EncodeWccData(&buf, resp.WccBefore, resp.WccAfter); err != nil {
		return nil, fmt.Errorf("encode wcc data: %w", err)
	}

	logger.Debug("Encoded MKDIR response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
