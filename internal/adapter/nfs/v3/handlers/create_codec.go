package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeCreateRequest decodes a CREATE request from XDR-encoded bytes.
//
// The CREATE request structure (RFC 1813 Section 3.3.8):
//
//	struct CREATE3args {
//	    diropargs3   where;
//	    createhow3   how;
//	};
//
// Decoding process:
//  1. Decode directory handle (opaque)
//  2. Decode filename (string)
//  3. Decode creation mode (uint32)
//  4. Based on mode:
//     - UNCHECKED/GUARDED: Decode sattr3
//     - EXCLUSIVE: Decode verifier (8 bytes)
//
// Parameters:
//   - data: XDR-encoded bytes
//
// Returns:
//   - *CreateRequest: Decoded request
//   - error: Decoding error if data is malformed
func DecodeCreateRequest(data []byte) (*CreateRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short for CREATE request: %d bytes", len(data))
	}

	reader := bytes.NewReader(data)

	// Decode directory handle
	dirHandle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}
	if dirHandle == nil {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Decode filename
	filename, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode filename: %w", err)
	}

	// Decode creation mode
	var mode uint32
	if err := binary.Read(reader, binary.BigEndian, &mode); err != nil {
		return nil, fmt.Errorf("decode creation mode: %w", err)
	}

	req := &CreateRequest{
		DirHandle: dirHandle,
		Filename:  filename,
		Mode:      mode,
	}

	// Decode mode-specific data
	switch mode {
	case types.CreateExclusive:
		// Decode verifier (8 bytes)
		var verf uint64
		if err := binary.Read(reader, binary.BigEndian, &verf); err != nil {
			return nil, fmt.Errorf("decode creation verifier: %w", err)
		}
		req.Verf = verf

	case types.CreateUnchecked, types.CreateGuarded:
		// Decode sattr3 (set attributes)
		attr, err := xdr.DecodeSetAttrs(reader)
		if err != nil {
			return nil, fmt.Errorf("decode attributes: %w", err)
		}
		req.Attr = attr

	default:
		return nil, fmt.Errorf("invalid creation mode: %d", mode)
	}

	return req, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the CreateResponse into XDR-encoded bytes.
//
// The response format (RFC 1813 Section 3.3.8):
//  1. Status code (4 bytes)
//  2. If success:
//     - Optional file handle
//     - Optional file attributes
//     - Directory WCC data
//  3. If failure:
//     - Directory WCC data
//
// Returns:
//   - []byte: XDR-encoded response
//   - error: Encoding error
func (resp *CreateResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Success case: Write file handle and attributes
	if resp.Status == types.NFS3OK {
		// Write optional file handle
		if err := xdr.EncodeOptionalOpaque(&buf, resp.FileHandle); err != nil {
			return nil, fmt.Errorf("encode file handle: %w", err)
		}

		// Write optional file attributes
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode file attributes: %w", err)
		}
	}

	// Write directory WCC data (both success and failure)
	if err := xdr.EncodeWccData(&buf, resp.DirBefore, resp.DirAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	return buf.Bytes(), nil
}
