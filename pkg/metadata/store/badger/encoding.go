package badger

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/block"
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
// File Data             "f:"     f:<uuid>                               File attrs (binary; JSON read-fallback)
// File Manifest         "fm:"    fm:<uuid>                              []block.ChunkRef (JSON)
// Parent Relationships  "p:"     p:<childUUID>                          parentUUID (bytes)
// Children Map          "c:"     c:<parentUUID>:<childName>             childUUID (bytes)
// Shares                "s:"     s:<shareName>                          shareData (JSON)
// Link Counts           "l:"     l:<uuid>                               uint32 (binary)
// Server Config         "cfg:"   cfg:server                             MetadataServerConfig (JSON)
// Filesystem Caps       "cap:"   cap:fs                                 FilesystemCapabilities (JSON)

const (
	prefixFile         = "f:"
	prefixFileManifest = "fm:" // file UUID -> block manifest ([]block.ChunkRef, JSON)
	prefixParent       = "p:"
	prefixChild        = "c:"
	prefixChildName    = "cn:" // (parentUUID, childUUID) -> child name (reverse edge)
	prefixShare        = "s:"
	prefixLinkCount    = "l:"
	prefixConfig       = "cfg:"
	prefixCapabilities = "cap:"
	prefixObjectID     = "obj:" // ObjectID -> file UUID
	prefixPayloadID    = "pl:"  // PayloadID (content ID) -> file UUID
)

// ============================================================================
// Key Generation Functions
// ============================================================================

// keyFile generates a key for file data: "f:<uuid>"
func keyFile(id uuid.UUID) []byte {
	return []byte(prefixFile + id.String())
}

// keyFileManifest generates the block-manifest key: "fm:<uuid>". The manifest
// (File.Blocks) lives here rather than in the f: attribute blob so an attr-only
// write (chmod/utimes/close/rename/xattr) does not rewrite the chunk list.
func keyFileManifest(id uuid.UUID) []byte {
	return []byte(prefixFileManifest + id.String())
}

// encodeManifest serializes a block manifest for the fm:<uuid> value. JSON keeps
// it self-describing and matches the bytes the legacy embedded field carried.
func encodeManifest(blocks []block.ChunkRef) ([]byte, error) {
	return json.Marshal(blocks)
}

// decodeManifest parses an fm:<uuid> value back into a block manifest.
func decodeManifest(b []byte) ([]block.ChunkRef, error) {
	var blocks []block.ChunkRef
	if err := json.Unmarshal(b, &blocks); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return blocks, nil
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

// keyPayloadID generates a key for the PayloadID secondary index:
// "pl:<payloadID>". Empty PayloadIDs MUST NOT be written through this helper --
// caller checks for "" first. PayloadID is the inode's stable content
// identifier (share/<inode-uuid>, see buildPayloadID), so one key maps to
// exactly one file and stays valid across rename/relink (#1166). The index lets
// GetFileByPayloadID resolve a file with an O(1) point lookup instead of a
// full-keyspace scan on the hot rollup persist path (#1435).
func keyPayloadID(p metadata.PayloadID) []byte {
	return []byte(prefixPayloadID + string(p))
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

// ---------------------------------------------------------------------------
// Inode (File) codec: self-describing length-prefixed binary, JSON read-fallback
// ---------------------------------------------------------------------------
//
// New writes use a compact binary format; json.Unmarshal was ~13% of flat CPU
// on the create/write hot path (an inode is decoded 4-5x per op), and it does
// reflection + field-name scanning + map allocation that a hand-written codec
// skips entirely (#1735).
//
// Format:
//
//	[0xD5 0xF5]  magic  (first byte != '{'/0x7b so the reader can tell binary
//	                     records from legacy JSON ones)
//	[0x01]       version
//	repeated fields, each:
//	  uvarint fieldID
//	  uvarint length
//	  length bytes of payload
//
// Self-describing / schema-evolution: every field is length-delimited and keyed
// by a stable fieldID. A decoder skips fieldIDs it does not know, and leaves the
// zero value for fieldIDs a record does not carry. So a new field is added by
// appending a new fieldID constant and its encode/decode arm -- old records
// (which lack it) still decode, and old binaries (which don't know it) skip it.
// Never renumber or reuse a retired fieldID.
//
// Aggregates (ACL, EAs, Blocks) keep JSON as their payload so their exact
// round-trip is inherited from the previous format, and they are only emitted
// when non-empty -- on the hot create/write path (nil ACL, no EAs, no Blocks)
// no JSON is touched at all.
const (
	fileMagic0  = 0xD5
	fileMagic1  = 0xF5
	fileVersion = 0x01
)

// File field IDs. Append-only; never reuse a number.
const (
	fID         = 1  // ID uuid.UUID (16 raw bytes)
	fShareName  = 2  // string
	fType       = 3  // FileType (uvarint)
	fMode       = 4  // uint32 (uvarint)
	fUID        = 5  // uint32 (uvarint)
	fGID        = 6  // uint32 (uvarint)
	fNlink      = 7  // uint32 (uvarint)
	fSize       = 8  // uint64 (uvarint)
	fAtime      = 9  // time.Time (MarshalBinary)
	fMtime      = 10 // time.Time
	fCtime      = 11 // time.Time
	fCreation   = 12 // time.Time
	fPayloadID  = 13 // string
	fLinkTarget = 14 // string
	fRdev       = 15 // uint64 (uvarint)
	fHidden     = 16 // bool (1 byte)
	fACL        = 17 // *acl.ACL (JSON)
	fEAs        = 18 // map[string][]byte (JSON)
	fIdempotenc = 19 // uint64 (uvarint)
	fBlocks     = 20 // []block.ChunkRef (JSON)
	fObjectID   = 21 // block.ObjectID (32 raw bytes)
	fDeletedAt  = 22 // *time.Time (MarshalBinary)
	fOrigPath   = 23 // string
	fDeletedBy  = 24 // string
	// Path (File.Path) is intentionally NOT encoded: it is derived from parent
	// edges on read (#1166). BlocksDirty is transient (json:"-"), also not stored.
)

func putField(dst []byte, id uint64, val []byte) []byte {
	dst = binary.AppendUvarint(dst, id)
	dst = binary.AppendUvarint(dst, uint64(len(val)))
	return append(dst, val...)
}

func putUvarintField(dst []byte, id, v uint64) []byte {
	return putField(dst, id, binary.AppendUvarint(nil, v))
}

func putStringField(dst []byte, id uint64, s string) []byte {
	if s == "" {
		return dst
	}
	return putField(dst, id, []byte(s))
}

func putTimeField(dst []byte, id uint64, t time.Time) ([]byte, error) {
	if t.IsZero() {
		return dst, nil
	}
	b, err := t.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return putField(dst, id, b), nil
}

func putJSONField(dst []byte, id uint64, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return putField(dst, id, b), nil
}

func encodeFile(file *metadata.File) ([]byte, error) {
	a := &file.FileAttr
	buf := make([]byte, 0, 256)
	buf = append(buf, fileMagic0, fileMagic1, fileVersion)

	idBytes := file.ID // uuid.UUID is [16]byte
	buf = putField(buf, fID, idBytes[:])
	buf = putStringField(buf, fShareName, file.ShareName)
	// Path is derived on read (#1166); never stored.
	buf = putUvarintField(buf, fType, uint64(a.Type))
	buf = putUvarintField(buf, fMode, uint64(a.Mode))
	buf = putUvarintField(buf, fUID, uint64(a.UID))
	buf = putUvarintField(buf, fGID, uint64(a.GID))
	buf = putUvarintField(buf, fNlink, uint64(a.Nlink))
	buf = putUvarintField(buf, fSize, a.Size)

	var err error
	if buf, err = putTimeField(buf, fAtime, a.Atime); err != nil {
		return nil, fmt.Errorf("encode atime: %w", err)
	}
	if buf, err = putTimeField(buf, fMtime, a.Mtime); err != nil {
		return nil, fmt.Errorf("encode mtime: %w", err)
	}
	if buf, err = putTimeField(buf, fCtime, a.Ctime); err != nil {
		return nil, fmt.Errorf("encode ctime: %w", err)
	}
	if buf, err = putTimeField(buf, fCreation, a.CreationTime); err != nil {
		return nil, fmt.Errorf("encode creation time: %w", err)
	}

	buf = putStringField(buf, fPayloadID, string(a.PayloadID))
	buf = putStringField(buf, fLinkTarget, a.LinkTarget)
	buf = putUvarintField(buf, fRdev, a.Rdev)
	if a.Hidden {
		buf = putField(buf, fHidden, []byte{1})
	}
	if a.ACL != nil {
		if buf, err = putJSONField(buf, fACL, a.ACL); err != nil {
			return nil, fmt.Errorf("encode acl: %w", err)
		}
	}
	if len(a.EAs) > 0 {
		if buf, err = putJSONField(buf, fEAs, a.EAs); err != nil {
			return nil, fmt.Errorf("encode eas: %w", err)
		}
	}
	buf = putUvarintField(buf, fIdempotenc, a.IdempotencyToken)
	// Blocks (the chunk manifest) are NOT written here — they live in the
	// sibling fm:<uuid> key so an attr-only write never rewrites the manifest.
	// The fBlocks field ID survives only in the decoder as a legacy read shim.
	if !a.ObjectID.IsZero() {
		oid := a.ObjectID // [32]byte
		buf = putField(buf, fObjectID, oid[:])
	}
	if a.DeletedAt != nil {
		if buf, err = putTimeField(buf, fDeletedAt, *a.DeletedAt); err != nil {
			return nil, fmt.Errorf("encode deleted_at: %w", err)
		}
	}
	buf = putStringField(buf, fOrigPath, a.OriginalPath)
	buf = putStringField(buf, fDeletedBy, a.DeletedBy)
	return buf, nil
}

// decodeFile dispatches on the first byte: legacy records are JSON (they start
// with '{' == 0x7b), new records carry the binary magic (0xD5). The JSON arm is
// a removable dual-read shim (modelled on the #1622 CAS->blocks read fallback):
// once every store has been rewritten with binary records it can be deleted.
func decodeFile(b []byte) (*metadata.File, error) {
	if len(b) > 0 && b[0] == '{' {
		return decodeFileJSON(b)
	}
	return decodeFileBinary(b)
}

// decodeFileJSON is the legacy read path. Removable once all badger stores hold
// binary records (#1735 dual-read shim).
func decodeFileJSON(b []byte) (*metadata.File, error) {
	var file metadata.File
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("failed to decode file (json): %w", err)
	}
	return &file, nil
}

func decodeFileBinary(b []byte) (*metadata.File, error) {
	if len(b) < 3 || b[0] != fileMagic0 || b[1] != fileMagic1 {
		return nil, fmt.Errorf("failed to decode file: bad binary header")
	}
	if b[2] != fileVersion {
		return nil, fmt.Errorf("failed to decode file: unsupported version %d", b[2])
	}
	r := b[3:]
	var file metadata.File
	a := &file.FileAttr
	for len(r) > 0 {
		id, n := binary.Uvarint(r)
		if n <= 0 {
			return nil, fmt.Errorf("failed to decode file: bad field id")
		}
		r = r[n:]
		l, n := binary.Uvarint(r)
		if n <= 0 {
			return nil, fmt.Errorf("failed to decode file: bad field length")
		}
		r = r[n:]
		if uint64(len(r)) < l {
			return nil, fmt.Errorf("failed to decode file: truncated field %d", id)
		}
		val := r[:l]
		r = r[l:]

		switch id {
		case fID:
			if len(val) != len(file.ID) {
				return nil, fmt.Errorf("failed to decode file: bad id length %d", len(val))
			}
			copy(file.ID[:], val)
		case fShareName:
			file.ShareName = string(val)
		case fType:
			a.Type = metadata.FileType(uvOf(val))
		case fMode:
			a.Mode = uint32(uvOf(val))
		case fUID:
			a.UID = uint32(uvOf(val))
		case fGID:
			a.GID = uint32(uvOf(val))
		case fNlink:
			a.Nlink = uint32(uvOf(val))
		case fSize:
			a.Size = uvOf(val)
		case fAtime:
			if err := a.Atime.UnmarshalBinary(val); err != nil {
				return nil, fmt.Errorf("decode atime: %w", err)
			}
		case fMtime:
			if err := a.Mtime.UnmarshalBinary(val); err != nil {
				return nil, fmt.Errorf("decode mtime: %w", err)
			}
		case fCtime:
			if err := a.Ctime.UnmarshalBinary(val); err != nil {
				return nil, fmt.Errorf("decode ctime: %w", err)
			}
		case fCreation:
			if err := a.CreationTime.UnmarshalBinary(val); err != nil {
				return nil, fmt.Errorf("decode creation time: %w", err)
			}
		case fPayloadID:
			a.PayloadID = metadata.PayloadID(val)
		case fLinkTarget:
			a.LinkTarget = string(val)
		case fRdev:
			a.Rdev = uvOf(val)
		case fHidden:
			a.Hidden = len(val) > 0 && val[0] != 0
		case fACL:
			if err := json.Unmarshal(val, &a.ACL); err != nil {
				return nil, fmt.Errorf("decode acl: %w", err)
			}
		case fEAs:
			if err := json.Unmarshal(val, &a.EAs); err != nil {
				return nil, fmt.Errorf("decode eas: %w", err)
			}
		case fIdempotenc:
			a.IdempotencyToken = uvOf(val)
		case fBlocks:
			// Legacy read shim: blobs written before the fm: manifest split
			// still embed the chunk list. Newer blobs never carry this field;
			// their manifest is loaded from fm:<uuid> (see loadManifest).
			if err := json.Unmarshal(val, &a.Blocks); err != nil {
				return nil, fmt.Errorf("decode blocks: %w", err)
			}
		case fObjectID:
			if len(val) != len(a.ObjectID) {
				return nil, fmt.Errorf("failed to decode file: bad object_id length %d", len(val))
			}
			copy(a.ObjectID[:], val)
		case fDeletedAt:
			var t time.Time
			if err := t.UnmarshalBinary(val); err != nil {
				return nil, fmt.Errorf("decode deleted_at: %w", err)
			}
			a.DeletedAt = &t
		case fOrigPath:
			a.OriginalPath = string(val)
		case fDeletedBy:
			a.DeletedBy = string(val)
		default:
			// Unknown field from a newer writer: skip it (already consumed).
		}
	}
	return &file, nil
}

// uvOf decodes a uvarint payload; a malformed payload yields 0, which for the
// scalar fields that use it is the correct zero value.
func uvOf(b []byte) uint64 {
	v, _ := binary.Uvarint(b)
	return v
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

// encodeInt64 encodes a signed 64-bit value (e.g. a Unix-nanos timestamp) as
// 8 big-endian bytes. decodeInt64 reverses it; a value shorter than 8 bytes
// (a legacy marker written before timestamps existed) decodes to 0.
func encodeInt64(value int64) []byte {
	bytes := make([]byte, 8)
	binary.BigEndian.PutUint64(bytes, uint64(value))
	return bytes
}

func decodeInt64(bytes []byte) int64 {
	if len(bytes) < 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(bytes))
}
