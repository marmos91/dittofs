package metadata

import (
	"encoding/hex"
	"time"
)

// ============================================================================
// Content-Addressed Types
// ============================================================================

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

// ============================================================================
// ObjectID Type for FileAttr
// ============================================================================

// ObjectID is a reference to a content-addressed Object.
// It's the ContentHash stored as a fixed-size array for embedding in FileAttr.
type ObjectID = ContentHash

// ZeroObjectID is an empty/unset ObjectID.
var ZeroObjectID = ObjectID{}

// ============================================================================
// BlockState
// ============================================================================

// BlockState represents the lifecycle state of a FileBlock.
//
// State machine: Dirty → Sealed → Uploading → Uploaded
//
//   - Dirty (0):     Receiving writes, NOT uploadable. Zero value is safe default
//     for legacy blocks deserialized without this field.
//   - Sealed (1):    Complete block, eligible for upload. Set when the next block
//     starts receiving writes, or when DataSize == BlockSize.
//   - Uploading (2): Upload in progress. Reverts to Sealed on failure.
//   - Uploaded (3):  Confirmed in block store (S3). Eligible for cache eviction.
//
// Write-after-upload resets: Uploaded → Dirty (clears Hash + BlockStoreKey).
type BlockState uint8

const (
	BlockStateDirty     BlockState = 0 // Receiving writes, NOT uploadable
	BlockStateSealed    BlockState = 1 // Complete, eligible for upload
	BlockStateUploading BlockState = 2 // Upload in progress
	BlockStateUploaded  BlockState = 3 // Confirmed in block store
)

// String returns the string representation of BlockState.
func (s BlockState) String() string {
	switch s {
	case BlockStateDirty:
		return "Dirty"
	case BlockStateSealed:
		return "Sealed"
	case BlockStateUploading:
		return "Uploading"
	case BlockStateUploaded:
		return "Uploaded"
	default:
		return "Unknown"
	}
}

// ============================================================================
// FileBlock
// ============================================================================

// FileBlock is the single block entity in DittoFS.
// Content-addressed: blocks with the same hash are shared across files for dedup.
//
// Lifecycle:
//  1. Created on write: ID=uuid, CachePath=path, State=Dirty
//  2. Sealed: block is complete (next block started or DataSize==BlockSize)
//  3. Uploading: upload in progress
//  4. Uploaded: BlockStoreKey set after background upload to S3/etc
//  5. Evictable: both CachePath and BlockStoreKey set, State=Uploaded
//  6. Evicted: CachePath cleared, data only in block store
type FileBlock struct {
	// ID is a stable UUID for this block.
	ID string

	// Hash is the SHA-256 of block data. Zero value means pending/incomplete.
	Hash ContentHash

	// DataSize is the actual bytes written in this block.
	DataSize uint32

	// CachePath is the local cache file path. Empty means not cached.
	CachePath string

	// BlockStoreKey is the opaque key in the block store (S3 key, FS path, etc.).
	// Empty means not uploaded.
	BlockStoreKey string

	// RefCount is the number of files referencing this block.
	RefCount uint32

	// LastAccess is used for LRU eviction.
	LastAccess time.Time

	// CreatedAt is when the block was created.
	CreatedAt time.Time

	// State is the block lifecycle state (Dirty → Sealed → Uploading → Uploaded).
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

// IsUploaded returns true if the block has been uploaded to the block store.
// Migration fallback: legacy blocks (State==0/Dirty) with BlockStoreKey set
// are treated as Uploaded — they were created before the state machine existed.
func (b *FileBlock) IsUploaded() bool {
	if b.State == BlockStateUploaded {
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

// IsDirty returns true if the block is receiving writes and not yet sealed.
func (b *FileBlock) IsDirty() bool {
	return b.State == BlockStateDirty
}

// IsSealed returns true if the block is complete and eligible for upload.
func (b *FileBlock) IsSealed() bool {
	return b.State == BlockStateSealed
}

// ============================================================================
// Errors
// ============================================================================

// ErrInvalidHash is returned when a hash string is malformed.
var ErrInvalidHash = &StoreError{
	Code:    ErrInvalidArgument,
	Message: "invalid content hash format",
}

// ErrObjectNotFound is returned when an object is not found.
var ErrObjectNotFound = &StoreError{
	Code:    ErrNotFound,
	Message: "object not found",
}

// ErrChunkNotFound is returned when a chunk is not found.
var ErrChunkNotFound = &StoreError{
	Code:    ErrNotFound,
	Message: "chunk not found",
}

// ErrBlockNotFound is returned when a block is not found.
var ErrBlockNotFound = &StoreError{
	Code:    ErrNotFound,
	Message: "block not found",
}

// ErrFileBlockNotFound is returned when a file block is not found.
var ErrFileBlockNotFound = &StoreError{
	Code:    ErrNotFound,
	Message: "file block not found",
}

// ErrObjectNotFinalized is returned when trying to read an unfinalized object.
var ErrObjectNotFinalized = &StoreError{
	Code:    ErrInvalidArgument,
	Message: "object not finalized",
}

// ErrDuplicateObject is returned when trying to create an object that already exists.
var ErrDuplicateObject = &StoreError{
	Code:    ErrAlreadyExists,
	Message: "object already exists",
}
