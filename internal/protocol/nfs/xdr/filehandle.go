package xdr

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ExtractFileID extracts the file identifier (inode number) from an NFS file handle.
//
// This is a thin wrapper around metadata.HandleToFileID() which provides the
// canonical implementation for converting file handles to inode numbers.
//
// Per RFC 1813 Section 3.3 (File Handles):
// File handles are opaque values used to identify files. The file ID (inode
// number) is used by NFS clients for cache coherency and to display in tools
// like ls -i and find.
//
// IMPORTANT: This function delegates to metadata.HandleToFileID() to ensure
// consistent inode generation across the entire system. Do not implement
// custom hashing logic here.
//
// Parameters:
//   - handle: Opaque file handle from client
//
// Returns:
//   - uint64: File identifier (inode number), or 0 if handle is empty
//
// See: metadata.HandleToFileID() for implementation details
func ExtractFileID(handle metadata.FileHandle) uint64 {
	return metadata.HandleToINode(handle)
}

// DecodeFileHandleFromRequest decodes a file handle from NFS request data.
//
// Most NFS v3 procedures include a file handle as the first field in their
// request parameters. This function uses XDR decoding to extract it.
//
// The file handle is encoded as XDR opaque data per RFC 1813:
//   - 4 bytes: length (big-endian uint32)
//   - N bytes: file handle data (max 64 bytes per NFS v3 spec)
//   - 0-3 bytes: padding to 4-byte boundary
//
// Parameters:
//   - data: Raw procedure request data (XDR-encoded)
//
// Returns:
//   - metadata.FileHandle: Decoded file handle, or nil if no handle present
//   - error: Decoding error or validation failure
//
// Special cases:
//   - Returns (nil, nil) if data is too short or handle length is 0
//   - Returns error if handle exceeds 64 bytes (RFC 1813 violation)
func DecodeFileHandleFromRequest(data []byte) (metadata.FileHandle, error) {
	// Need at least 4 bytes for handle length field
	if len(data) < 4 {
		return nil, nil // No handle present (e.g., NULL procedure)
	}

	// Use XDR decoder to parse opaque file handle
	reader := bytes.NewReader(data)
	handleBytes, err := DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode file handle: %w", err)
	}

	// No handle present (empty opaque data)
	if len(handleBytes) == 0 {
		return nil, nil
	}

	// Validate handle length (NFS v3 handles must be <= 64 bytes per RFC 1813)
	if len(handleBytes) > 64 {
		return nil, fmt.Errorf("handle length %d exceeds maximum 64 bytes", len(handleBytes))
	}

	return metadata.FileHandle(handleBytes), nil
}
