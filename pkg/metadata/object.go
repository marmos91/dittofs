package metadata

import (
	"encoding/hex"
	"time"
)

// ============================================================================
// Content-Addressed Object Types
// ============================================================================
//
// DittoFS uses a three-level hierarchy for content-addressed storage:
//
//   FileAttr (ObjectID) → Object → ObjectChunk → ObjectBlock
//
// This enables:
//   - File-level deduplication (identical files share same Object)
//   - Chunk-level deduplication (64MB chunks can be shared)
//   - Block-level deduplication (4MB blocks can be shared)
//   - Hard links naturally share Objects
//   - Integrity verification via content hashes
//
// Note: These types are separate from the existing ChunkInfo/SliceInfo/BlockRef
// types in chunks.go, which are used for the slice-based write model. The
// content-addressed types here are for deduplication and integrity verification.
//
// ============================================================================

// HashSize is the size of content hashes (SHA-256 = 32 bytes).
const HashSize = 32

// ContentHash represents a SHA-256 hash of content.
// Used as content-addressed identifier for Objects, Chunks, and Blocks.
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
// Object
// ============================================================================

// Object represents a content-addressed file object.
//
// An Object is identified by its content hash (SHA-256 of the entire file content
// or a Merkle root of chunk hashes). Multiple FileAttrs can reference the same
// Object (hard links), enabling automatic deduplication.
//
// Object lifecycle:
//  1. Created when file content is first written
//  2. RefCount incremented for each FileAttr referencing it
//  3. RefCount decremented when FileAttr is removed
//  4. Deleted (along with chunks/blocks) when RefCount reaches 0
type Object struct {
	// ID is the content hash of the object.
	// Computed as SHA-256 of the complete file content, or as a Merkle root
	// of chunk hashes for large files.
	ID ContentHash

	// Size is the total size of the object in bytes.
	Size uint64

	// RefCount is the number of FileAttrs referencing this Object.
	// When RefCount reaches 0, the Object and its chunks/blocks can be deleted.
	RefCount uint32

	// ChunkCount is the number of chunks in this object.
	// ChunkCount = ceil(Size / ChunkSize)
	ChunkCount uint32

	// CreatedAt is when the object was first created.
	CreatedAt time.Time

	// Finalized indicates whether the object's hash has been computed.
	// During active writes, the object is not finalized.
	// On file close/commit, the final hash is computed and Finalized is set to true.
	Finalized bool
}

// NewObject creates a new Object with the given hash and size.
func NewObject(id ContentHash, size uint64) *Object {
	return &Object{
		ID:         id,
		Size:       size,
		RefCount:   1,
		ChunkCount: uint32((size + ChunkSize - 1) / ChunkSize),
		CreatedAt:  time.Now(),
		Finalized:  true,
	}
}

// NewPendingObject creates a new unfinalized Object for active writes.
// The hash will be computed when the object is finalized.
func NewPendingObject() *Object {
	return &Object{
		RefCount:  1,
		CreatedAt: time.Now(),
		Finalized: false,
	}
}

// ============================================================================
// ObjectChunk
// ============================================================================

// ObjectChunk represents a content-addressed chunk within an Object.
//
// Chunks are 64MB segments of a file. Each chunk has its own content hash
// (computed from its blocks or directly from content), enabling chunk-level
// deduplication.
//
// Note: This is separate from ChunkInfo in chunks.go, which tracks slices
// for the write model. ObjectChunk is for content-addressed deduplication.
type ObjectChunk struct {
	// ObjectID is the parent object's content hash.
	ObjectID ContentHash

	// Index is the chunk index within the object (0, 1, 2, ...).
	Index uint32

	// Hash is the content hash of this chunk.
	// Computed as SHA-256 of chunk content or Merkle root of block hashes.
	Hash ContentHash

	// Size is the actual size of this chunk in bytes.
	// May be less than ChunkSize for the last chunk.
	Size uint32

	// BlockCount is the number of blocks in this chunk.
	// BlockCount = ceil(Size / DefaultBlockSize)
	BlockCount uint32

	// RefCount is the number of Objects referencing this chunk.
	// Enables chunk-level deduplication when different objects share chunks.
	RefCount uint32
}

// NewObjectChunk creates a new ObjectChunk.
func NewObjectChunk(objectID ContentHash, index uint32, hash ContentHash, size uint32) *ObjectChunk {
	return &ObjectChunk{
		ObjectID:   objectID,
		Index:      index,
		Hash:       hash,
		Size:       size,
		BlockCount: uint32((uint64(size) + DefaultBlockSize - 1) / DefaultBlockSize),
		RefCount:   1,
	}
}

// ============================================================================
// ObjectBlock
// ============================================================================

// ObjectBlock represents a content-addressed block within a Chunk.
//
// Blocks are 4MB segments that are the unit of:
//   - Storage in the block store (S3, filesystem, etc.)
//   - Deduplication (blocks with same hash are stored once)
//   - Transfer (uploads and downloads operate on blocks)
//
// Note: This is separate from BlockRef in chunks.go, which references blocks
// by ID for the slice model. ObjectBlock is content-addressed with hashes.
type ObjectBlock struct {
	// ChunkHash is the parent chunk's content hash.
	// Used for lookup and association.
	ChunkHash ContentHash

	// Index is the block index within the chunk (0, 1, 2, ...).
	Index uint32

	// Hash is the content hash of this block (SHA-256 of block data).
	Hash ContentHash

	// Size is the actual size of this block in bytes.
	// May be less than DefaultBlockSize for the last block.
	Size uint32

	// RefCount is the number of Chunks referencing this block.
	// Enables block-level deduplication across different chunks/files.
	RefCount uint32

	// UploadedAt is when the block was uploaded to the block store.
	// Zero value indicates the block hasn't been uploaded yet.
	UploadedAt time.Time
}

// NewObjectBlock creates a new ObjectBlock.
func NewObjectBlock(chunkHash ContentHash, index uint32, hash ContentHash, size uint32) *ObjectBlock {
	return &ObjectBlock{
		ChunkHash: chunkHash,
		Index:     index,
		Hash:      hash,
		Size:      size,
		RefCount:  1,
	}
}

// IsUploaded returns true if the block has been uploaded to the block store.
func (b *ObjectBlock) IsUploaded() bool {
	return !b.UploadedAt.IsZero()
}

// MarkUploaded marks the block as uploaded.
func (b *ObjectBlock) MarkUploaded() {
	b.UploadedAt = time.Now()
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
