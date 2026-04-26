package blockstore

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BlockSize is the size of a single block (8MB). This is the single source of
// truth -- all packages should reference this constant instead of defining
// their own copies.
const BlockSize = 8 * 1024 * 1024

// HashSize is the size of content hashes (BLAKE3 = 32 bytes).
const HashSize = 32

// ContentHash represents a BLAKE3-256 content hash. Width is 32 bytes,
// matching SHA-256 wire-compat so legacy metadata deserializes unchanged.
type ContentHash [HashSize]byte

// String returns the hex-encoded hash string.
func (h ContentHash) String() string {
	return hex.EncodeToString(h[:])
}

// CASKey returns the content-addressed key in scheme "blake3:{hex}".
// Used for the local CAS key format and the S3 x-amz-meta-content-hash header.
func (h ContentHash) CASKey() string {
	return "blake3:" + hex.EncodeToString(h[:])
}

// IsZero returns true if the hash is all zeros (uninitialized).
func (h ContentHash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

// ParseContentHash parses a hex-encoded hash string.
func ParseContentHash(s string) (ContentHash, error) {
	var h ContentHash
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, err
	}
	if len(b) != HashSize {
		return h, ErrInvalidHash
	}
	copy(h[:], b)
	return h, nil
}

// BlockState is the lifecycle state of a FileBlock: Pending -> Syncing -> Remote.
//
//   - Pending (0): RefCount >= 1, not yet uploaded. Safe zero value for legacy
//     rows deserialized without this field.
//   - Syncing (1): Claimed by a syncer goroutine; upload in flight.
//   - Remote (2):  PUT + metadata-txn confirmed; eligible for local eviction.
//
// Write-after-sync resets Remote -> Pending (clears Hash + BlockStoreKey).
type BlockState uint8

const (
	BlockStatePending BlockState = 0
	BlockStateSyncing BlockState = 1
	BlockStateRemote  BlockState = 2
)

// String returns the string representation of BlockState.
func (s BlockState) String() string {
	switch s {
	case BlockStatePending:
		return "Pending"
	case BlockStateSyncing:
		return "Syncing"
	case BlockStateRemote:
		return "Remote"
	default:
		return fmt.Sprintf("BlockState(%d)", s)
	}
}

// FileBlock is the single block entity in DittoFS. Content-addressed:
// blocks with the same hash are shared across files for dedup.
//
// Lifecycle:
//  1. Pending  — created on write (LocalPath set, BlockStoreKey empty).
//  2. Syncing  — claim batch flipped State and stamped LastSyncAttemptAt.
//  3. Remote   — PUT + metadata-txn confirmed (BlockStoreKey set).
//  4. Evicted  — LocalPath cleared; data lives only in the remote store.
type FileBlock struct {
	// ID is a stable UUID for this block.
	ID string

	// Hash is the BLAKE3-256 of block data. Zero value means pending/incomplete.
	Hash ContentHash

	// DataSize is the actual bytes written in this block.
	DataSize uint32

	// LocalPath is the local file path. Empty means not stored locally.
	LocalPath string

	// BlockStoreKey is the opaque key in the remote block store (S3 key, FS path, etc.).
	// Empty means not synced to remote.
	BlockStoreKey string

	// RefCount is the number of files referencing this block.
	RefCount uint32

	// LastAccess is used for LRU eviction.
	LastAccess time.Time

	// LastSyncAttemptAt is the time the syncer last claimed this block.
	// The restart-recovery janitor requeues Syncing rows whose attempt
	// exceeds syncer.claim_timeout. Zero value means never attempted.
	LastSyncAttemptAt time.Time `json:"last_sync_attempt_at,omitempty"`

	// CreatedAt is when the block was created.
	CreatedAt time.Time

	// State is the block lifecycle state. Zero value (Pending) is the safe
	// default for legacy blocks.
	State BlockState `json:"state"`
}

// NewFileBlock creates a new pending FileBlock with the given ID and local path.
func NewFileBlock(id string, localPath string) *FileBlock {
	now := time.Now()
	return &FileBlock{
		ID:         id,
		LocalPath:  localPath,
		RefCount:   1,
		LastAccess: now,
		CreatedAt:  now,
	}
}

// IsRemote returns true if the block has been synced to the remote block store.
// Dual-read fallback (D-21): legacy zero-valued rows (State==Pending) that
// already carry a BlockStoreKey were uploaded under the legacy non-CAS path
// and must still be treated as Remote during the dual-read window.
func (b *FileBlock) IsRemote() bool {
	if b.State == BlockStateRemote {
		return true
	}
	return b.State == BlockStatePending && b.BlockStoreKey != ""
}

// HasLocalFile returns true if the block exists in the local store.
func (b *FileBlock) HasLocalFile() bool {
	return b.LocalPath != ""
}

// IsFinalized returns true if the block's upload is complete (State==Remote).
func (b *FileBlock) IsFinalized() bool {
	return b.State == BlockStateRemote
}

// IsDirty returns true if the block is Pending and has never been uploaded
// (no BlockStoreKey) — distinguishing a freshly-written block from a
// Pending block that already exists remotely (legacy path).
func (b *FileBlock) IsDirty() bool {
	return b.State == BlockStatePending && b.BlockStoreKey == ""
}

// IsLocal returns true if the block is Pending with data on the local
// filesystem — i.e. eligible for the syncer to claim and upload.
func (b *FileBlock) IsLocal() bool {
	return b.State == BlockStatePending && b.LocalPath != ""
}

// FormatStoreKey returns the block store key (S3 object key) for a block.
// Format: "{payloadID}/block-{blockIdx}".
func FormatStoreKey(payloadID string, blockIdx uint64) string {
	return fmt.Sprintf("%s/block-%d", payloadID, blockIdx)
}

// ParseStoreKey extracts the payloadID and block index from a store key.
// Store key format: "{payloadID}/block-{blockIdx}".
// Returns ("", 0, false) if the key format is invalid.
func ParseStoreKey(storeKey string) (payloadID string, blockIdx uint64, ok bool) {
	idx := strings.LastIndex(storeKey, "/block-")
	if idx < 0 || idx == 0 {
		return "", 0, false
	}
	payloadID = storeKey[:idx]
	blockIdx, err := strconv.ParseUint(storeKey[idx+len("/block-"):], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return payloadID, blockIdx, true
}

// FormatCASKey returns the flat S3 object key for a content-addressed block.
// Format: "cas/{hex[0:2]}/{hex[2:4]}/{hex}". Two-level fanout caps the
// top-level prefix count at 256 and bounds per-prefix file count predictably.
// Mirror to ParseCASKey. See BSCAS-01.
func FormatCASKey(h ContentHash) string {
	hexStr := hex.EncodeToString(h[:])
	return "cas/" + hexStr[0:2] + "/" + hexStr[2:4] + "/" + hexStr
}

// ParseCASKey parses an S3 object key produced by FormatCASKey and returns
// the embedded ContentHash. Returns ErrCASKeyMalformed wrapped with the
// offending input on any shape, length, or hex error.
// Symmetric to FormatCASKey. See BSCAS-01 / D-29.
func ParseCASKey(key string) (ContentHash, error) {
	const prefix = "cas/"
	if !strings.HasPrefix(key, prefix) {
		return ContentHash{}, fmt.Errorf("%w: missing %q prefix in %q", ErrCASKeyMalformed, prefix, key)
	}
	rest := key[len(prefix):]
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return ContentHash{}, fmt.Errorf("%w: expected 3 segments after prefix in %q", ErrCASKeyMalformed, key)
	}
	shard1, shard2, hexStr := parts[0], parts[1], parts[2]
	if len(shard1) != 2 || len(shard2) != 2 {
		return ContentHash{}, fmt.Errorf("%w: shard segments must be 2 hex chars in %q", ErrCASKeyMalformed, key)
	}
	if len(hexStr) != HashSize*2 {
		return ContentHash{}, fmt.Errorf("%w: hex hash must be %d chars in %q", ErrCASKeyMalformed, HashSize*2, key)
	}
	if hexStr[0:2] != shard1 || hexStr[2:4] != shard2 {
		return ContentHash{}, fmt.Errorf("%w: shard prefix does not match hash in %q", ErrCASKeyMalformed, key)
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return ContentHash{}, fmt.Errorf("%w: %v", ErrCASKeyMalformed, err)
	}
	var h ContentHash
	copy(h[:], raw)
	return h, nil
}

// ParseBlockID extracts the payloadID and block index from an internal
// blockID string. BlockID format: "{payloadID}/{blockIdx}" where payloadID
// may itself contain '/' characters (only the LAST '/' separates the index).
//
// Example: "export/docs/report.pdf/7" -> ("export/docs/report.pdf", 7, nil).
//
// Returns a wrapped error for malformed inputs: missing separator,
// empty trailing index, or non-numeric index. Callers that previously
// relied on silent zero-value returns must now propagate the error.
func ParseBlockID(blockID string) (payloadID string, blockIdx uint64, err error) {
	lastSlash := strings.LastIndex(blockID, "/")
	if lastSlash <= 0 {
		return "", 0, fmt.Errorf("parse blockID %q: missing payloadID/idx separator: %w", blockID, ErrInvalidPayloadID)
	}
	payloadID = blockID[:lastSlash]
	idxStr := blockID[lastSlash+1:]
	if idxStr == "" {
		return "", 0, fmt.Errorf("parse blockID %q: empty block index: %w", blockID, ErrInvalidPayloadID)
	}
	blockIdx, parseErr := strconv.ParseUint(idxStr, 10, 64)
	if parseErr != nil {
		return "", 0, fmt.Errorf("parse blockID %q: invalid block index: %w", blockID, parseErr)
	}
	return payloadID, blockIdx, nil
}

// KeyBelongsToFile checks if a store key belongs to the given payloadID.
// Store key format: "{payloadID}/block-{blockIdx}".
func KeyBelongsToFile(key, payloadID string) bool {
	prefix := payloadID + "/block-"
	return len(key) > len(prefix) && key[:len(prefix)] == prefix
}

// ParseBlockIdx extracts the block index from a store key for a known payloadID.
// Returns 0 if the key format is invalid.
func ParseBlockIdx(key, payloadID string) uint64 {
	prefix := payloadID + "/block-"
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return 0
	}
	idx, err := strconv.ParseUint(key[len(prefix):], 10, 64)
	if err != nil {
		return 0
	}
	return idx
}
