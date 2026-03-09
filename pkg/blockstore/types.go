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

// HashSize is the size of content hashes (SHA-256 = 32 bytes).
const HashSize = 32

// ContentHash represents a SHA-256 hash of content.
type ContentHash [HashSize]byte

// String returns the hex-encoded hash string.
func (h ContentHash) String() string {
	return hex.EncodeToString(h[:])
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

// BlockState represents the lifecycle state of a FileBlock.
//
// State machine: Dirty -> Local -> Syncing -> Remote
//
//   - Dirty (0):   Receiving writes, NOT syncable. Zero value is safe default
//     for legacy blocks deserialized without this field.
//   - Local (1):   Complete block on local disk, eligible for sync to remote.
//     Set when the next block starts receiving writes, or when DataSize == BlockSize.
//   - Syncing (2): Sync to remote store in progress. Reverts to Local on failure.
//   - Remote (3):  Confirmed in remote block store. Eligible for cache eviction.
//
// Write-after-sync resets: Remote -> Dirty (clears Hash + BlockStoreKey).
type BlockState uint8

const (
	BlockStateDirty   BlockState = 0 // Receiving writes, NOT syncable
	BlockStateLocal   BlockState = 1 // Complete, on disk, eligible for sync to remote
	BlockStateSyncing BlockState = 2 // Sync to remote in progress
	BlockStateRemote  BlockState = 3 // Confirmed in remote block store
)

// String returns the string representation of BlockState.
func (s BlockState) String() string {
	switch s {
	case BlockStateDirty:
		return "Dirty"
	case BlockStateLocal:
		return "Local"
	case BlockStateSyncing:
		return "Syncing"
	case BlockStateRemote:
		return "Remote"
	default:
		return "Unknown"
	}
}

// FileBlock is the single block entity in DittoFS.
// Content-addressed: blocks with the same hash are shared across files for dedup.
//
// Lifecycle:
//  1. Created on write: ID=uuid, CachePath=path, State=Dirty
//  2. Local: block is complete (next block started or DataSize==BlockSize)
//  3. Syncing: sync to remote store in progress
//  4. Remote: BlockStoreKey set after background sync to remote store
//  5. Remote + cached: both CachePath and BlockStoreKey set, State=Remote
//  6. Evicted: CachePath cleared, data only in remote store
type FileBlock struct {
	// ID is a stable UUID for this block.
	ID string

	// Hash is the SHA-256 of block data. Zero value means pending/incomplete.
	Hash ContentHash

	// DataSize is the actual bytes written in this block.
	DataSize uint32

	// CachePath is the local cache file path. Empty means not cached.
	CachePath string

	// BlockStoreKey is the opaque key in the remote block store (S3 key, FS path, etc.).
	// Empty means not synced to remote.
	BlockStoreKey string

	// RefCount is the number of files referencing this block.
	RefCount uint32

	// LastAccess is used for LRU eviction.
	LastAccess time.Time

	// CreatedAt is when the block was created.
	CreatedAt time.Time

	// State is the block lifecycle state (Dirty -> Local -> Syncing -> Remote).
	// Zero value (Dirty) is the safe default for legacy blocks.
	State BlockState `json:"state"`
}

// NewFileBlock creates a new pending FileBlock with the given ID and cache path.
func NewFileBlock(id string, cachePath string) *FileBlock {
	now := time.Now()
	return &FileBlock{
		ID:         id,
		CachePath:  cachePath,
		RefCount:   1,
		LastAccess: now,
		CreatedAt:  now,
	}
}

// IsRemote returns true if the block has been synced to the remote block store.
// Migration fallback: legacy blocks (State==0/Dirty) with BlockStoreKey set
// are treated as Remote -- they were created before the state machine existed.
func (b *FileBlock) IsRemote() bool {
	if b.State == BlockStateRemote {
		return true
	}
	// Migration fallback for legacy blocks without State field
	return b.State == BlockStateDirty && b.BlockStoreKey != ""
}

// IsCached returns true if the block exists in the local cache.
func (b *FileBlock) IsCached() bool {
	return b.CachePath != ""
}

// IsFinalized returns true if the block's hash has been computed.
func (b *FileBlock) IsFinalized() bool {
	return !b.Hash.IsZero()
}

// IsDirty returns true if the block is receiving writes and not yet complete.
func (b *FileBlock) IsDirty() bool {
	return b.State == BlockStateDirty
}

// IsLocal returns true if the block is complete and eligible for sync to remote.
func (b *FileBlock) IsLocal() bool {
	return b.State == BlockStateLocal
}

// BlockRef references a single block in storage.
type BlockRef struct {
	// Key is the full block key in storage.
	// Format: "{payloadID}/block-{blockIdx}"
	Key string

	// Size is the actual size of this block (may be < BlockSize for last block).
	Size uint32
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
