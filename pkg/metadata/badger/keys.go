package badger

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Database Key Namespace Design
// ==============================
//
// BadgerDB is a key-value store, so we use prefixed keys to organize different
// data types into logical namespaces. This design:
//   - Prevents key collisions between different data types
//   - Enables efficient range scans (e.g., all children of a directory)
//   - Makes the database structure self-documenting
//   - Supports future extensions without schema changes
//
// Key Namespace Prefixes:
//
// Data Type             Prefix   Key Format                              Value Type
// ==================================================================================
// File Attributes       "f:"     f:<handleKey>                          fileData (JSON)
// Parent Relationships  "p:"     p:<childHandleKey>                     parentHandle (bytes)
// Children Map          "c:"     c:<parentHandleKey>:<childName>        childHandle (bytes)
// Shares                "s:"     s:<shareName>                          shareData (JSON)
// Link Counts           "l:"     l:<handleKey>                          uint32 (binary)
// Device Numbers        "d:"     d:<handleKey>                          deviceNumber (JSON)
// Pending Writes        "w:"     w:<operationID>                        WriteOperation (JSON)
// Mount Sessions        "m:"     m:<shareName>|<clientAddr>             ShareSession (JSON)
// Handle Mapping        "hmap:"  hmap:<handleKey>                       shareName:fullPath (string)
// Server Config         "cfg:"   cfg:server                             MetadataServerConfig (JSON)
// Filesystem Caps       "cap:"   cap:fs                                 FilesystemCapabilities (JSON)
//
// Key Design Rationale:
//
// 1. File Attributes (f:)
//    - One entry per file/directory
//    - Handle is unique, so no collisions
//    - Point lookup: O(1)
//
// 2. Parent Relationships (p:)
//    - Maps each file to its parent directory
//    - Used for upward traversal (e.g., getting parent for "..")
//    - Point lookup: O(1)
//
// 3. Children Map (c:)
//    - Denormalized: one entry per child (not one map per directory)
//    - Enables efficient range scans: all keys with prefix "c:<parent>:"
//    - Format: c:<parentHandle>:<childName> → childHandle
//    - List children: range scan from "c:<parent>:" to "c:<parent>:\xff"
//    - O(n) where n = number of children (unavoidable for directory listing)
//
// 4. Shares (s:)
//    - One entry per share configuration
//    - Share names are unique
//    - Point lookup: O(1)
//
// 5. Link Counts (l:)
//    - Tracks number of hard links to each file
//    - Used to determine when to delete content (count reaches 0)
//    - Point lookup: O(1)
//
// 6. Device Numbers (d:)
//    - Only for block/char device special files
//    - Stores major/minor device numbers
//    - Sparse: most files don't have entries here
//    - Point lookup: O(1)
//
// 7. Pending Writes (w:)
//    - Tracks in-flight two-phase write operations
//    - Operation IDs are UUIDs (unique)
//    - Used for PrepareWrite/CommitWrite coordination
//    - Point lookup: O(1)
//
// 8. Mount Sessions (m:)
//    - Tracks which clients have mounted which shares
//    - Used for administrative monitoring (DUMP procedure)
//    - Composite key: shareName + "|" + clientAddr
//    - List all sessions: range scan "m:" to "m:\xff"
//    - O(n) where n = number of active sessions
//
// 9. Handle Mapping (hmap:)
//    - Reverse mapping for hash-based file handles
//    - Only used for handles that don't fit in 64 bytes
//    - Maps handle → original path
//    - Sparse: most files use path-based handles
//    - Point lookup: O(1)
//
// 10. Server Config (cfg:)
//     - Singleton: one configuration per server
//     - Point lookup: O(1)
//
// 11. Filesystem Capabilities (cap:)
//     - Singleton: one set of capabilities per server
//     - Point lookup: O(1)

const (
	// prefixFile is the key prefix for file attributes
	prefixFile = "f:"

	// prefixParent is the key prefix for parent relationships
	prefixParent = "p:"

	// prefixChild is the key prefix for children mappings
	prefixChild = "c:"

	// prefixShare is the key prefix for share configurations
	prefixShare = "s:"

	// prefixLinkCount is the key prefix for link counts
	prefixLinkCount = "l:"

	// prefixDeviceNumber is the key prefix for device numbers
	prefixDeviceNumber = "d:"

	// prefixPendingWrite is the key prefix for pending write operations
	prefixPendingWrite = "w:"

	// prefixSession is the key prefix for mount sessions
	prefixSession = "m:"

	// prefixHandleMapping is the key prefix for hash-based handle reverse mapping
	prefixHandleMapping = "hmap:"

	// prefixConfig is the key prefix for server configuration
	prefixConfig = "cfg:"

	// prefixCapabilities is the key prefix for filesystem capabilities
	prefixCapabilities = "cap:"
)

// Key generation functions
// ========================
// These functions construct database keys for different data types.
// They ensure consistent key formatting and prevent errors from manual
// string concatenation.

// keyFile generates a key for file attributes.
//
// Format: "f:<handleKey>"
//
// Parameters:
//   - handle: The file handle
//
// Returns:
//   - []byte: Database key for file attributes
func keyFile(handle metadata.FileHandle) []byte {
	return []byte(prefixFile + handleToKey(handle))
}

// keyParent generates a key for parent relationship.
//
// Format: "p:<childHandleKey>"
//
// Parameters:
//   - childHandle: The child file handle
//
// Returns:
//   - []byte: Database key for parent relationship
func keyParent(childHandle metadata.FileHandle) []byte {
	return []byte(prefixParent + handleToKey(childHandle))
}

// keyChild generates a key for a child entry in a directory.
//
// Format: "c:<parentHandleKey>:<childName>"
//
// This format enables efficient range scans to list all children of a directory.
// Range: ["c:<parent>:", "c:<parent>:\xff")
//
// Parameters:
//   - parentHandle: The parent directory handle
//   - childName: The name of the child
//
// Returns:
//   - []byte: Database key for child entry
func keyChild(parentHandle metadata.FileHandle, childName string) []byte {
	return []byte(prefixChild + handleToKey(parentHandle) + ":" + childName)
}

// keyChildPrefix generates a key prefix for range scanning children.
//
// Format: "c:<parentHandleKey>:"
//
// Use this to scan all children of a directory with range query:
//
//	from: keyChildPrefix(handle)
//	to:   keyChildPrefixEnd(handle)
//
// Parameters:
//   - parentHandle: The parent directory handle
//
// Returns:
//   - []byte: Database key prefix for children
func keyChildPrefix(parentHandle metadata.FileHandle) []byte {
	return []byte(prefixChild + handleToKey(parentHandle) + ":")
}

// keyChildPrefixEnd generates the end key for range scanning children.
//
// This creates a key that is lexicographically greater than all possible
// child entries for the given parent, used as the exclusive upper bound
// for range scans.
//
// Parameters:
//   - parentHandle: The parent directory handle
//
// Returns:
//   - []byte: Database key range end for children
func keyChildPrefixEnd(parentHandle metadata.FileHandle) []byte {
	prefix := keyChildPrefix(parentHandle)
	// Append 0xFF to create a key greater than all children
	return append(prefix, 0xFF)
}

// keyShare generates a key for share configuration.
//
// Format: "s:<shareName>"
//
// Parameters:
//   - shareName: The share name
//
// Returns:
//   - []byte: Database key for share configuration
func keyShare(shareName string) []byte {
	return []byte(prefixShare + shareName)
}

// keySharePrefix returns the prefix for range scanning all shares.
//
// Format: "s:"
//
// Returns:
//   - []byte: Database key prefix for all shares
func keySharePrefix() []byte {
	return []byte(prefixShare)
}

// keyLinkCount generates a key for file link count.
//
// Format: "l:<handleKey>"
//
// Parameters:
//   - handle: The file handle
//
// Returns:
//   - []byte: Database key for link count
func keyLinkCount(handle metadata.FileHandle) []byte {
	return []byte(prefixLinkCount + handleToKey(handle))
}

// keyDeviceNumber generates a key for device numbers.
//
// Format: "d:<handleKey>"
//
// Parameters:
//   - handle: The file handle
//
// Returns:
//   - []byte: Database key for device numbers
func keyDeviceNumber(handle metadata.FileHandle) []byte {
	return []byte(prefixDeviceNumber + handleToKey(handle))
}

// keyPendingWrite generates a key for pending write operations.
//
// Format: "w:<operationID>"
//
// Parameters:
//   - operationID: The write operation ID (typically UUID)
//
// Returns:
//   - []byte: Database key for pending write
func keyPendingWrite(operationID string) []byte {
	return []byte(prefixPendingWrite + operationID)
}

// keySession generates a key for mount session.
//
// Format: "m:<shareName>|<clientAddr>"
//
// The "|" separator is chosen because it's unlikely to appear in share names
// or client addresses, and makes the key human-readable.
//
// Parameters:
//   - shareName: The share name
//   - clientAddr: The client address
//
// Returns:
//   - []byte: Database key for session
func keySession(shareName, clientAddr string) []byte {
	return []byte(fmt.Sprintf("%s%s|%s", prefixSession, shareName, clientAddr))
}

// keySessionPrefix returns the prefix for range scanning all sessions.
//
// Format: "m:"
//
// Returns:
//   - []byte: Database key prefix for all sessions
func keySessionPrefix() []byte {
	return []byte(prefixSession)
}

// keyHandleMapping generates a key for hash-based handle reverse mapping.
//
// Format: "hmap:<handleKey>"
//
// This is only used for handles that exceeded the NFS size limit and were
// converted to hash-based format. The value is the original "shareName:fullPath".
//
// Parameters:
//   - handle: The hash-based file handle
//
// Returns:
//   - []byte: Database key for handle mapping
func keyHandleMapping(handle metadata.FileHandle) []byte {
	return []byte(prefixHandleMapping + handleToKey(handle))
}

// keyServerConfig generates the key for server configuration.
//
// Format: "cfg:server"
//
// This is a singleton key - there's only one server config.
//
// Returns:
//   - []byte: Database key for server configuration
func keyServerConfig() []byte {
	return []byte(prefixConfig + "server")
}

// keyFilesystemCapabilities generates the key for filesystem capabilities.
//
// Format: "cap:fs"
//
// This is a singleton key - there's only one set of capabilities.
//
// Returns:
//   - []byte: Database key for filesystem capabilities
func keyFilesystemCapabilities() []byte {
	return []byte(prefixCapabilities + "fs")
}
