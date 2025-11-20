package badger

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Serialization Strategy
// ======================
//
// BadgerDB stores data as raw bytes, so we need to serialize Go structs before
// storing and deserialize when reading. We use different strategies based on
// data type complexity:
//
// 1. JSON Encoding (Complex Types)
//    - File attributes, shares, sessions, configs
//    - Pros: Human-readable, flexible schema evolution, easy debugging
//    - Cons: Larger size, slower than binary
//    - Usage: Where flexibility and debuggability matter more than raw performance
//
// 2. Binary Encoding (Simple Types)
//    - Link counts (uint32), file handles (raw bytes)
//    - Pros: Compact, fast
//    - Cons: Not human-readable, rigid schema
//    - Usage: Where performance and size matter, schema is stable
//
// Future Optimizations:
// - Could use MessagePack or Protocol Buffers for better performance/size
// - Could use Gob for native Go type support
// - For now, JSON provides the best balance of debuggability and functionality

// fileData holds the internal representation of file metadata with share tracking.
//
// This structure extends FileAttr with the share name, which is needed to:
//   - Enforce share-level policies (e.g., read-only shares)
//   - Generate deterministic file handles (which include share name)
//   - Track which share a file belongs to for management operations
type fileData struct {
	// Attr contains the protocol-agnostic file attributes
	Attr *metadata.FileAttr `json:"attr"`

	// ShareName tracks which share this file belongs to
	ShareName string `json:"share_name"`
}

// shareData holds the internal representation of a share configuration.
//
// This structure combines the share configuration (access rules, options)
// with the root directory handle that serves as the entry point for all
// filesystem operations within the share.
type shareData struct {
	// Share contains the share configuration and access control rules
	Share metadata.Share `json:"share"`

	// RootHandle is the file handle for the share's root directory
	// This is the entry point for all filesystem operations within the share
	RootHandle metadata.FileHandle `json:"root_handle"`
}

// deviceNumber stores major and minor device numbers for special files.
//
// This is only populated for block and character device files. Regular files,
// directories, symlinks, sockets, and FIFOs don't have device numbers.
type deviceNumber struct {
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
}

// Encoding functions
// ==================
// These functions convert Go types to byte slices for storage in BadgerDB.

// encodeFileData serializes fileData to JSON bytes.
//
// This is used for storing complete file metadata including attributes and
// share tracking information.
//
// Parameters:
//   - data: The file data to encode
//
// Returns:
//   - []byte: JSON-encoded bytes
//   - error: Encoding error if serialization fails
func encodeFileData(data *fileData) ([]byte, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to encode file data: %w", err)
	}
	return bytes, nil
}

// decodeFileData deserializes fileData from JSON bytes.
//
// This is the inverse of encodeFileData, used when reading file metadata
// from the database.
//
// Parameters:
//   - bytes: JSON-encoded file data
//
// Returns:
//   - *fileData: Decoded file data
//   - error: Decoding error if deserialization fails
func decodeFileData(bytes []byte) (*fileData, error) {
	var data fileData
	if err := json.Unmarshal(bytes, &data); err != nil {
		return nil, fmt.Errorf("failed to decode file data: %w", err)
	}
	return &data, nil
}

// decodeShareData deserializes shareData from JSON bytes.
//
// Parameters:
//   - bytes: JSON-encoded share data
//
// Returns:
//   - *shareData: Decoded share data
//   - error: Decoding error if deserialization fails
func decodeShareData(bytes []byte) (*shareData, error) {
	var data shareData
	if err := json.Unmarshal(bytes, &data); err != nil {
		return nil, fmt.Errorf("failed to decode share data: %w", err)
	}
	return &data, nil
}

// encodeDeviceNumber serializes deviceNumber to JSON bytes.
//
// Device numbers are only stored for block and character device special files.
// The JSON format makes it easy to extend with additional fields if needed.
//
// Parameters:
//   - dev: The device number to encode
//
// Returns:
//   - []byte: JSON-encoded bytes
//   - error: Encoding error if serialization fails
func encodeDeviceNumber(dev *deviceNumber) ([]byte, error) {
	bytes, err := json.Marshal(dev)
	if err != nil {
		return nil, fmt.Errorf("failed to encode device number: %w", err)
	}
	return bytes, nil
}

// encodeServerConfig serializes MetadataServerConfig to JSON bytes.
//
// Server configuration is stored as a singleton and includes global settings
// that apply across all shares and operations.
//
// Parameters:
//   - config: The server config to encode
//
// Returns:
//   - []byte: JSON-encoded bytes
//   - error: Encoding error if serialization fails
func encodeServerConfig(config *metadata.MetadataServerConfig) ([]byte, error) {
	bytes, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to encode server config: %w", err)
	}
	return bytes, nil
}

// decodeServerConfig deserializes MetadataServerConfig from JSON bytes.
//
// Parameters:
//   - bytes: JSON-encoded config
//
// Returns:
//   - *metadata.MetadataServerConfig: Decoded config
//   - error: Decoding error if deserialization fails
func decodeServerConfig(bytes []byte) (*metadata.MetadataServerConfig, error) {
	var config metadata.MetadataServerConfig
	if err := json.Unmarshal(bytes, &config); err != nil {
		return nil, fmt.Errorf("failed to decode server config: %w", err)
	}
	return &config, nil
}

// encodeFilesystemCapabilities serializes FilesystemCapabilities to JSON bytes.
//
// Capabilities are stored as a singleton and define what the filesystem supports
// (max file size, features, limits, etc.).
//
// Parameters:
//   - caps: The capabilities to encode
//
// Returns:
//   - []byte: JSON-encoded bytes
//   - error: Encoding error if serialization fails
func encodeFilesystemCapabilities(caps *metadata.FilesystemCapabilities) ([]byte, error) {
	bytes, err := json.Marshal(caps)
	if err != nil {
		return nil, fmt.Errorf("failed to encode filesystem capabilities: %w", err)
	}
	return bytes, nil
}

// decodeFilesystemCapabilities deserializes FilesystemCapabilities from JSON bytes.
//
// Parameters:
//   - bytes: JSON-encoded capabilities
//
// Returns:
//   - *metadata.FilesystemCapabilities: Decoded capabilities
//   - error: Decoding error if deserialization fails
func decodeFilesystemCapabilities(bytes []byte) (*metadata.FilesystemCapabilities, error) {
	var caps metadata.FilesystemCapabilities
	if err := json.Unmarshal(bytes, &caps); err != nil {
		return nil, fmt.Errorf("failed to decode filesystem capabilities: %w", err)
	}
	return &caps, nil
}

// Binary encoding functions for simple types
// ===========================================
// These provide efficient encoding for simple numeric types.

// encodeUint32 serializes a uint32 to 4 bytes using big-endian encoding.
//
// Big-endian is chosen for consistency with network byte order and to make
// values comparable in lexicographic ordering (which BadgerDB uses for keys).
//
// Parameters:
//   - value: The uint32 value to encode
//
// Returns:
//   - []byte: 4-byte big-endian representation
func encodeUint32(value uint32) []byte {
	bytes := make([]byte, 4)
	binary.BigEndian.PutUint32(bytes, value)
	return bytes
}

// decodeUint32 deserializes a uint32 from 4 bytes using big-endian encoding.
//
// Parameters:
//   - bytes: 4-byte big-endian encoded value
//
// Returns:
//   - uint32: Decoded value
//   - error: Error if bytes length is not 4
func decodeUint32(bytes []byte) (uint32, error) {
	if len(bytes) != 4 {
		return 0, fmt.Errorf("invalid uint32 bytes: expected 4 bytes, got %d", len(bytes))
	}
	return binary.BigEndian.Uint32(bytes), nil
}

// FileHandle encoding is just a direct byte copy since it's already []byte
// No encoding/decoding functions needed - just use handle directly as []byte
