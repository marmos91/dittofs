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

// encodeFile serializes a File to JSON bytes.
//
// This stores the complete file metadata including:
//   - Identity: UUID, ShareName, Path
//   - Attributes: Type, Mode, UID, GID, Size, timestamps, ContentID, etc.
//
// Because File embeds FileAttr, all fields appear at the root level in JSON.
//
// Example JSON structure:
//
//	{
//	  "id": "550e8400-e29b-41d4-a716-446655440000",
//	  "share_name": "/export",
//	  "path": "/documents/report.pdf",
//	  "type": 0,
//	  "mode": 420,
//	  "uid": 1000,
//	  "gid": 1000,
//	  "size": 4096,
//	  "atime": "2025-11-21T14:00:00Z",
//	  "mtime": "2025-11-21T14:00:00Z",
//	  "ctime": "2025-11-21T14:00:00Z",
//	  "content_id": "export/documents/report.pdf"
//	}
//
// Parameters:
//   - file: The file to encode
//
// Returns:
//   - []byte: JSON-encoded bytes
//   - error: Encoding error if serialization fails
func encodeFile(file *metadata.File) ([]byte, error) {
	bytes, err := json.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("failed to encode file: %w", err)
	}
	return bytes, nil
}

// decodeFile deserializes a File from JSON bytes.
//
// This is the inverse of encodeFile, used when reading file metadata
// from the database.
//
// Parameters:
//   - bytes: JSON-encoded file data
//
// Returns:
//   - *metadata.File: Decoded file with UUID, share, path, and all attributes
//   - error: Decoding error if deserialization fails
func decodeFile(bytes []byte) (*metadata.File, error) {
	var file metadata.File
	if err := json.Unmarshal(bytes, &file); err != nil {
		return nil, fmt.Errorf("failed to decode file: %w", err)
	}
	return &file, nil
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

// encodeShareData serializes shareData to JSON bytes.
//
// Parameters:
//   - data: The share data to encode
//
// Returns:
//   - []byte: JSON-encoded bytes
//   - error: Encoding error if serialization fails
func encodeShareData(data *shareData) ([]byte, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to encode share data: %w", err)
	}
	return bytes, nil
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
