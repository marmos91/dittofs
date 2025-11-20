package xdr

import (
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
