package badger

import (
	"github.com/google/uuid"
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
// UUID-Based File Identification:
//
// Files are identified by UUID v4 (random), which provides:
//   - Always under 64-byte NFS handle limit (shareName:uuid ≈ 45 bytes)
//   - No path length limitations
//   - Stable across renames (UUID doesn't change when file is moved)
//   - Collision resistance without coordination
//
// Key Namespace Prefixes:
//
// Data Type             Prefix   Key Format                              Value Type
// ==================================================================================
// File Data             "f:"     f:<uuid>                               File (JSON)
// Parent Relationships  "p:"     p:<childUUID>                          parentUUID (bytes)
// Children Map          "c:"     c:<parentUUID>:<childName>             childUUID (bytes)
// Shares                "s:"     s:<shareName>                          shareData (JSON)
// Link Counts           "l:"     l:<uuid>                               uint32 (binary)
// Device Numbers        "d:"     d:<uuid>                               deviceNumber (JSON)
// Server Config         "cfg:"   cfg:server                             MetadataServerConfig (JSON)
// Filesystem Caps       "cap:"   cap:fs                                 FilesystemCapabilities (JSON)
//
// Key Design Rationale:
//
// 1. File Data (f:)
//    - One entry per file/directory
//    - Stores complete File struct (ID, ShareName, Path, and all FileAttr fields)
//    - UUID is unique, so no collisions
//    - Point lookup by UUID: O(1)
//    - Example: f:550e8400-e29b-41d4-a716-446655440000
//
// 2. Parent Relationships (p:)
//    - Maps each file UUID to its parent directory UUID
//    - Used for upward traversal (e.g., getting parent for "..")
//    - Point lookup: O(1)
//    - Example: p:550e8400... → parent-uuid-bytes
//
// 3. Children Map (c:)
//    - Denormalized: one entry per child (not one map per directory)
//    - Enables efficient range scans: all keys with prefix "c:<parentUUID>:"
//    - Format: c:<parentUUID>:<childName> → childUUID
//    - List children: range scan from "c:<parentUUID>:" to "c:<parentUUID>:\xff"
//    - O(n) where n = number of children (unavoidable for directory listing)
//    - Example: c:parent-uuid:file.txt → child-uuid-bytes
//
// 4. Shares (s:)
//    - One entry per share configuration
//    - Share names are unique
//    - Point lookup: O(1)
//    - Example: s:/export
//
// 5. Link Counts (l:)
//    - Tracks number of hard links to each file
//    - Used to determine when to delete content (count reaches 0)
//    - Point lookup: O(1)
//    - Example: l:550e8400...
//
// 6. Device Numbers (d:)
//    - Only for block/char device special files
//    - Stores major/minor device numbers
//    - Sparse: most files don't have entries here
//    - Point lookup: O(1)
//    - Example: d:550e8400...
//
// 7. Server Config (cfg:)
//    - Singleton: one configuration per server
//    - Point lookup: O(1)
//
// 8. Filesystem Capabilities (cap:)
//    - Singleton: one set of capabilities per server
//    - Point lookup: O(1)
//
// Removed Prefixes (no longer needed with UUIDs):
//   - Handle Mapping (hmap:) - UUIDs always fit in 64 bytes, no hash needed
//   - Pending Writes (w:) - Not implemented in current version
//   - Mount Sessions (m:) - Not implemented in current version

const (
	// prefixFile is the key prefix for file data (File struct with UUID)
	prefixFile = "f:"

	// prefixParent is the key prefix for parent relationships (UUID → parentUUID)
	prefixParent = "p:"

	// prefixChild is the key prefix for children mappings (parentUUID:name → childUUID)
	prefixChild = "c:"

	// prefixShare is the key prefix for share configurations
	prefixShare = "s:"

	// prefixLinkCount is the key prefix for link counts (UUID → count)
	prefixLinkCount = "l:"

	// prefixDeviceNumber is the key prefix for device numbers (UUID → device)
	prefixDeviceNumber = "d:"

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

// keyFile generates a key for file data.
//
// Format: "f:<uuid>"
// Example: "f:550e8400-e29b-41d4-a716-446655440000"
//
// Parameters:
//   - id: The file UUID
//
// Returns:
//   - []byte: Database key for file data
func keyFile(id uuid.UUID) []byte {
	return []byte(prefixFile + id.String())
}

// keyParent generates a key for parent relationship.
//
// Format: "p:<childUUID>"
// Example: "p:550e8400-e29b-41d4-a716-446655440000"
//
// Parameters:
//   - childID: The child file UUID
//
// Returns:
//   - []byte: Database key for parent relationship
func keyParent(childID uuid.UUID) []byte {
	return []byte(prefixParent + childID.String())
}

// keyChild generates a key for a child entry in a directory.
//
// Format: "c:<parentUUID>:<childName>"
// Example: "c:550e8400-e29b-41d4-a716-446655440000:report.pdf"
//
// This format enables efficient range scans to list all children of a directory.
// Range: ["c:<parentUUID>:", "c:<parentUUID>:\xff")
//
// Parameters:
//   - parentID: The parent directory UUID
//   - childName: The name of the child
//
// Returns:
//   - []byte: Database key for child entry
func keyChild(parentID uuid.UUID, childName string) []byte {
	return []byte(prefixChild + parentID.String() + ":" + childName)
}

// keyChildPrefix generates a key prefix for range scanning children.
//
// Format: "c:<parentUUID>:"
// Example: "c:550e8400-e29b-41d4-a716-446655440000:"
//
// Use this to scan all children of a directory with range query:
//
//	from: keyChildPrefix(parentID)
//	to:   append(keyChildPrefix(parentID), 0xff)
//
// Parameters:
//   - parentID: The parent directory UUID
//
// Returns:
//   - []byte: Database key prefix for children
func keyChildPrefix(parentID uuid.UUID) []byte {
	return []byte(prefixChild + parentID.String() + ":")
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

// keyLinkCount generates a key for file link count.
//
// Format: "l:<uuid>"
// Example: "l:550e8400-e29b-41d4-a716-446655440000"
//
// Parameters:
//   - id: The file UUID
//
// Returns:
//   - []byte: Database key for link count
func keyLinkCount(id uuid.UUID) []byte {
	return []byte(prefixLinkCount + id.String())
}

// keyDeviceNumber generates a key for device numbers.
//
// Format: "d:<uuid>"
// Example: "d:550e8400-e29b-41d4-a716-446655440000"
//
// Parameters:
//   - id: The file UUID
//
// Returns:
//   - []byte: Database key for device numbers
func keyDeviceNumber(id uuid.UUID) []byte {
	return []byte(prefixDeviceNumber + id.String())
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
