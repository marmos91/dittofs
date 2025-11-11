package xdr

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// extractFileID extracts the file identifier from an NFS file handle.
// Per RFC 1813 Section 3.3 (File Handles):
// File handles are opaque values used to identify files. This implementation
// uses the first 8 bytes of the handle as a uint64 file ID.
//
// Handle format: [file_id:8 bytes][...other data...]
//
// Parameters:
//   - handle: Opaque file handle from client
//
// Returns:
//   - uint64: File identifier, or 0 if handle is too short (invalid)
//
// Production note: Returning 0 for invalid handles will cause lookup failures,
// which is the desired behavior for malformed handles.
func ExtractFileID(handle metadata.FileHandle) uint64 {
	if len(handle) < 8 {
		// Invalid handle: too short to contain file ID
		return 0
	}
	return binary.BigEndian.Uint64(handle[:8])
}
