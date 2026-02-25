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

// DecodeMknodRequest decodes a MKNOD request from XDR-encoded bytes.
//
// The MKNOD request has a discriminated union structure based on file type
// (RFC 1813 Section 3.3.11):
//
//	union mknoddata3 switch (ftype3 type) {
//	    case NF3CHR:
//	    case NF3BLK:
//	        devicedata3 device;
//	    case NF3SOCK:
//	    case NF3FIFO:
//	        sattr3 pipe_attributes;
//	    default:
//	        void;
//	};
//
// Decoding process:
//  1. Read parent directory handle (variable length with padding)
//  2. Read special file name (variable length string with padding)
//  3. Read file type (discriminated union)
//  4. Based on type:
//     - For CHR/BLK: Read attributes + device spec (major/minor)
//     - For SOCK/FIFO: Read attributes only
//
// Parameters:
//   - data: XDR-encoded bytes containing the mknod request
//
// Returns:
//   - *MknodRequest: The decoded request
//   - error: Decoding error if data is malformed or incomplete
func DecodeMknodRequest(data []byte) (*MknodRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)
	req := &MknodRequest{}

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
	req.DirHandle = handle

	// ========================================================================
	// Decode special file name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}
	req.Name = name

	// ========================================================================
	// Decode file type (discriminated union)
	// ========================================================================

	var fileType uint32
	if err := binary.Read(reader, binary.BigEndian, &fileType); err != nil {
		return nil, fmt.Errorf("decode file type: %w", err)
	}
	req.Type = fileType

	// ========================================================================
	// Decode type-specific data based on discriminated union
	// ========================================================================

	switch fileType {
	case types.NF3CHR, types.NF3BLK:
		// Character and block devices: attributes + device spec
		attr, err := xdr.DecodeSetAttrs(reader)
		if err != nil {
			return nil, fmt.Errorf("decode device attributes: %w", err)
		}
		req.Attr = attr

		// Decode device spec (major/minor numbers)
		if err := binary.Read(reader, binary.BigEndian, &req.Spec.SpecData1); err != nil {
			return nil, fmt.Errorf("decode major device number: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &req.Spec.SpecData2); err != nil {
			return nil, fmt.Errorf("decode minor device number: %w", err)
		}

	case types.NF3SOCK, types.NF3FIFO:
		// Sockets and FIFOs: attributes only (no device spec)
		attr, err := xdr.DecodeSetAttrs(reader)
		if err != nil {
			return nil, fmt.Errorf("decode pipe attributes: %w", err)
		}
		req.Attr = attr

		// Device spec is not present for sockets and FIFOs
		req.Spec = DeviceSpec{SpecData1: 0, SpecData2: 0}

	default:
		// Invalid file type - this should have been caught by validation
		// but handle gracefully during decoding
		return nil, fmt.Errorf("invalid file type for MKNOD: %d (expected CHR/BLK/SOCK/FIFO)", fileType)
	}

	var mode uint32
	if req.Attr != nil && req.Attr.Mode != nil {
		mode = *req.Attr.Mode
	}

	logger.Debug("Decoded MKNOD request", "handle_len", len(handle), "name", name, "type", fileType, "mode", fmt.Sprintf("%o", mode), "major", req.Spec.SpecData1, "minor", req.Spec.SpecData2)

	return req, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the MknodResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The MKNOD response has the following XDR structure (RFC 1813 Section 3.3.11):
//
//	struct MKNOD3resok {
//	    post_op_fh3   obj;           // New file handle
//	    post_op_attr  obj_attributes;
//	    wcc_data      dir_wcc;       // Parent directory WCC
//	};
//
//	struct MKNOD3resfail {
//	    wcc_data      dir_wcc;
//	};
//
// Encoding process:
//  1. Write status code (4 bytes)
//  2. If success (NFS3OK):
//     a. Write optional new file handle
//     b. Write optional new file attributes
//     c. Write WCC data for parent directory
//  3. If failure:
//     a. Write WCC data for parent directory (best effort)
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
func (resp *MknodResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Success case: Write handle and attributes
	// ========================================================================

	if resp.Status == types.NFS3OK {
		// Write new file handle (post_op_fh3 - optional)
		if err := xdr.EncodeOptionalOpaque(&buf, resp.FileHandle); err != nil {
			return nil, fmt.Errorf("encode file handle: %w", err)
		}

		// Write new file attributes (post_op_attr - optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode file attributes: %w", err)
		}
	}

	// ========================================================================
	// Write WCC data for parent directory (both success and failure)
	// ========================================================================

	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.
	if err := xdr.EncodeWccData(&buf, resp.DirAttrBefore, resp.DirAttrAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded MKNOD response", "bytes", buf.Len(), "status", resp.Status)
	return buf.Bytes(), nil
}
