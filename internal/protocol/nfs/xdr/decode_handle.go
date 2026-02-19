package xdr

import (
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// DecodeFileHandleFromReader decodes an XDR opaque file handle from an io.Reader.
//
// This consolidates the repeated file handle decoding pattern found across all
// NFS v3 codec files. It reads variable-length opaque data per RFC 4506 and
// validates the handle length per RFC 1813 (max 64 bytes).
//
// Parameters:
//   - reader: Input stream positioned at start of opaque file handle
//
// Returns:
//   - metadata.FileHandle: Decoded handle, or nil if length is 0
//   - error: Decoding error or validation failure
func DecodeFileHandleFromReader(reader io.Reader) (metadata.FileHandle, error) {
	handleBytes, err := DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode file handle: %w", err)
	}

	if len(handleBytes) == 0 {
		return nil, nil
	}

	// Validate handle length per RFC 1813 (NFS v3 handles must be <= 64 bytes)
	if len(handleBytes) > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", len(handleBytes))
	}

	return metadata.FileHandle(handleBytes), nil
}
