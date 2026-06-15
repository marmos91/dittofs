package badger

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Database Key Namespace Design
// ============================================================================
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

const (
	prefixFile         = "f:"
	prefixParent       = "p:"
	prefixChild        = "c:"
	prefixChildName    = "cn:" // (parentUUID, childUUID) -> child name (reverse edge)
	prefixShare        = "s:"
	prefixLinkCount    = "l:"
	prefixDeviceNumber = "d:"
	prefixConfig       = "cfg:"
	prefixCapabilities = "cap:"
	prefixObjectID     = "obj:" // ObjectID -> file UUID
)

// ============================================================================
// Key Generation Functions
// ============================================================================

// keyFile generates a key for file data: "f:<uuid>"
func keyFile(id uuid.UUID) []byte {
	return []byte(prefixFile + id.String())
}

// keyParent generates a key for parent relationship: "p:<childUUID>"
func keyParent(childID uuid.UUID) []byte {
	return []byte(prefixParent + childID.String())
}

// keyChild generates a key for a child entry: "c:<parentUUID>:<childName>"
func keyChild(parentID uuid.UUID, childName string) []byte {
	return []byte(prefixChild + parentID.String() + ":" + childName)
}

// keyChildPrefix generates a key prefix for range scanning children: "c:<parentUUID>:"
func keyChildPrefix(parentID uuid.UUID) []byte {
	return []byte(prefixChild + parentID.String() + ":")
}

// keyChildName generates the reverse-edge key "cn:<parentUUID>:<childUUID>"
// whose value is the name under which child is linked into parent. It gives
// derivePath an O(1) child->name lookup instead of scanning every c:<parent>:*
// entry per path component (#1166). One key exists per directed (parent, child)
// edge, so a hard link to a different directory writes a distinct key and never
// disturbs the canonical parent's edge.
func keyChildName(parentID, childID uuid.UUID) []byte {
	return []byte(prefixChildName + parentID.String() + ":" + childID.String())
}

// keyChildNamePrefix generates a key prefix for range scanning a directory's
// reverse edges: "cn:<parentUUID>:"
func keyChildNamePrefix(parentID uuid.UUID) []byte {
	return []byte(prefixChildName + parentID.String() + ":")
}

// keyShare generates a key for share configuration: "s:<shareName>"
func keyShare(shareName string) []byte {
	return []byte(prefixShare + shareName)
}

// keyLinkCount generates a key for file link count: "l:<uuid>"
func keyLinkCount(id uuid.UUID) []byte {
	return []byte(prefixLinkCount + id.String())
}

// keyServerConfig generates the key for server configuration: "cfg:server"
func keyServerConfig() []byte {
	return []byte(prefixConfig + "server")
}

// keyFilesystemCapabilities generates the key for filesystem capabilities: "cap:fs"
func keyFilesystemCapabilities() []byte {
	return []byte(prefixCapabilities + "fs")
}

// keyObjectID generates a key for the ObjectID secondary index:
// "obj:<hex>". Zero-valued ObjectIDs MUST NOT be written through this
// helper -- caller checks IsZero() first.
func keyObjectID(h metadata.ContentHash) []byte {
	return []byte(prefixObjectID + h.String())
}

// ============================================================================
// Internal Types
// ============================================================================

// shareData holds share configuration with its root directory handle.
type shareData struct {
	Share      metadata.Share      `json:"share"`
	RootHandle metadata.FileHandle `json:"root_handle"`
}

// ============================================================================
// JSON Encoding/Decoding
// ============================================================================

func encodeFile(file *metadata.File) ([]byte, error) {
	// Path is always derived from parent edges on read (#1166), never trusted
	// from storage. Zero it before serializing so no badger row persists a
	// stale/misleading path (e.g. the pre-move path on a rename). decodeFile
	// tolerates the absent field; GetFile overwrites Path via derivePath.
	if file.Path != "" {
		clone := *file
		clone.Path = ""
		file = &clone
	}
	bytes, err := json.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("failed to encode file: %w", err)
	}
	return bytes, nil
}

func decodeFile(bytes []byte) (*metadata.File, error) {
	var file metadata.File
	if err := json.Unmarshal(bytes, &file); err != nil {
		return nil, fmt.Errorf("failed to decode file: %w", err)
	}
	return &file, nil
}

func encodeShareData(data *shareData) ([]byte, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to encode share data: %w", err)
	}
	return bytes, nil
}

func decodeShareData(bytes []byte) (*shareData, error) {
	var data shareData
	if err := json.Unmarshal(bytes, &data); err != nil {
		return nil, fmt.Errorf("failed to decode share data: %w", err)
	}
	return &data, nil
}

func encodeServerConfig(config *metadata.MetadataServerConfig) ([]byte, error) {
	bytes, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to encode server config: %w", err)
	}
	return bytes, nil
}

func decodeServerConfig(bytes []byte) (*metadata.MetadataServerConfig, error) {
	var config metadata.MetadataServerConfig
	if err := json.Unmarshal(bytes, &config); err != nil {
		return nil, fmt.Errorf("failed to decode server config: %w", err)
	}
	return &config, nil
}

func encodeFilesystemCapabilities(caps *metadata.FilesystemCapabilities) ([]byte, error) {
	bytes, err := json.Marshal(caps)
	if err != nil {
		return nil, fmt.Errorf("failed to encode filesystem capabilities: %w", err)
	}
	return bytes, nil
}

func decodeFilesystemCapabilities(bytes []byte) (*metadata.FilesystemCapabilities, error) {
	var caps metadata.FilesystemCapabilities
	if err := json.Unmarshal(bytes, &caps); err != nil {
		return nil, fmt.Errorf("failed to decode filesystem capabilities: %w", err)
	}
	return &caps, nil
}

// ============================================================================
// Binary Encoding/Decoding
// ============================================================================

func encodeUint32(value uint32) []byte {
	bytes := make([]byte, 4)
	binary.BigEndian.PutUint32(bytes, value)
	return bytes
}

func decodeUint32(bytes []byte) (uint32, error) {
	if len(bytes) != 4 {
		return 0, fmt.Errorf("invalid uint32 bytes: expected 4 bytes, got %d", len(bytes))
	}
	return binary.BigEndian.Uint32(bytes), nil
}
