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
// XDR Encoding and Decoding
// ============================================================================

// DecodeFsStatRequest decodes an FSSTAT request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.18 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: The XDR-encoded request bytes
//
// Returns:
//   - *FsStatRequest: The decoded request
//   - error: Returns error if decoding fails due to malformed data
//
// Errors returned:
//   - "data too short": Input buffer is too small for basic structure
//   - "read handle length": Failed to read the handle length field
//   - "invalid handle length": Handle length exceeds RFC 1813 limit (64 bytes)
//   - "read handle": Failed to read the handle data
func DecodeFsStatRequest(data []byte) (*FsStatRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short for FSSTAT request: got %d bytes, need at least 4", len(data))
	}

	reader := bytes.NewReader(data)

	handle, err := xdr.DecodeFileHandleFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode file handle: %w", err)
	}

	logger.Debug("Decoded FSSTAT request", "handle_len", len(handle))
	return &FsStatRequest{Handle: handle}, nil
}

// Encode serializes an FSSTAT response to XDR-encoded bytes.
//
// The encoding follows RFC 1813 Section 3.3.18 specifications:
//  1. Status (4 bytes, big-endian uint32)
//  2. Post-op attributes (optional, present flag + attributes if Status == types.NFS3OK)
//  3. Filesystem statistics (only if Status == types.NFS3OK):
//     - Total bytes (8 bytes)
//     - Free bytes (8 bytes)
//     - Available bytes (8 bytes)
//     - Total files (8 bytes)
//     - Free files (8 bytes)
//     - Available files (8 bytes)
//     - Invariant time (4 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Returns:
//   - []byte: The XDR-encoded response
//   - error: Returns error if encoding fails (typically due to I/O issues)
//
// Errors returned:
//   - "write status": Failed to write the status field
//   - "write *": Failed to write various fields
//   - "encode file attributes": Failed to encode the metadata.FileAttr structure
func (resp *FsStatResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, return early with just the status
	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding FSSTAT error response", "status", resp.Status)
		return buf.Bytes(), nil
	}

	// Write post-op attributes (present flag + attributes)
	if resp.Attr != nil {
		// Attributes present (1)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("write attr present flag: %w", err)
		}
		if err := xdr.EncodeFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode file attributes: %w", err)
		}
	} else {
		// Attributes not present (0)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("write attr absent flag: %w", err)
		}
	}

	// Write filesystem statistics
	if err := binary.Write(&buf, binary.BigEndian, resp.Tbytes); err != nil {
		return nil, fmt.Errorf("write total bytes: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Fbytes); err != nil {
		return nil, fmt.Errorf("write free bytes: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Abytes); err != nil {
		return nil, fmt.Errorf("write available bytes: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Tfiles); err != nil {
		return nil, fmt.Errorf("write total files: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Ffiles); err != nil {
		return nil, fmt.Errorf("write free files: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Afiles); err != nil {
		return nil, fmt.Errorf("write available files: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Invarsec); err != nil {
		return nil, fmt.Errorf("write invarsec: %w", err)
	}

	logger.Debug("Encoded FSSTAT response", "bytes", buf.Len())
	return buf.Bytes(), nil
}
