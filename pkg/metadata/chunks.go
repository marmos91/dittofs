package metadata

import (
	"time"
)

// ============================================================================
// Chunk/Slice/Block Constants
// ============================================================================

const (
	// ChunkSize is the size of a chunk in bytes (64MB).
	// Files are divided into chunks for metadata organization and lazy loading.
	ChunkSize = 64 * 1024 * 1024

	// DefaultBlockSize is the default block size for storage (4MB).
	// Each block becomes a single object in the block store (S3, filesystem).
	DefaultBlockSize = 4 * 1024 * 1024

	// MinBlockSize is the minimum allowed block size (1MB).
	MinBlockSize = 1 * 1024 * 1024

	// MaxBlockSize is the maximum allowed block size (16MB).
	MaxBlockSize = 16 * 1024 * 1024

	// DefaultMaxSlicesPerChunk triggers compaction when exceeded.
	DefaultMaxSlicesPerChunk = 16
)

// ============================================================================
// Chunk/Slice/Block Types
// ============================================================================

// ChunkInfo stores slices for a 64MB chunk of a file.
//
// Each file is divided into chunks of ChunkSize bytes. Each chunk contains
// one or more slices representing write operations. Slices may overlap -
// the newest slice wins on read.
//
// The Chunk/Slice/Block model avoids read-modify-write on S3:
//   - Write 1KB to 100GB file: Create 1KB slice, NOT re-upload 100GB
//   - Compaction merges fragmented slices in background
//
// Storage in BadgerDB:
//
//	Key: chunks:{file_handle}:{chunk_index}
//	Value: Serialized ChunkInfo
type ChunkInfo struct {
	// FileHandle references the parent file.
	FileHandle []byte

	// ChunkIndex is the chunk number (0, 1, 2, ... = offset / ChunkSize).
	ChunkIndex uint32

	// Slices contains all slices for this chunk, ordered by CreatedAt (newest first).
	// On read, iterate from newest to oldest - first match wins.
	Slices []SliceInfo

	// Version is incremented on every modification for optimistic locking.
	Version uint64
}

// SliceInfo represents a contiguous write operation within a chunk.
//
// A slice captures a single write or a coalesced group of adjacent writes.
// Slices may overlap - on read, the newest slice (by CreatedAt) wins.
//
// Key insight: Creating a new slice is copy-on-write. Old data is preserved
// until compaction, enabling efficient overwrites without read-modify-write.
type SliceInfo struct {
	// ID uniquely identifies this slice (UUID).
	ID string

	// Offset is the byte offset within the chunk (0 to ChunkSize-1).
	Offset uint32

	// Length is the size of this slice in bytes.
	Length uint32

	// Blocks contains references to the blocks holding this slice's data.
	// For small slices, this is typically a single block.
	// For large slices (>BlockSize), data is split across multiple blocks.
	Blocks []BlockRef

	// CreatedAt determines newest-wins ordering for overlapping slices.
	CreatedAt time.Time
}

// BlockRef references an immutable block in the block store.
//
// Blocks are the physical storage units - each block is a single object
// in S3 or a single file in the filesystem block store.
//
// Blocks are immutable: once written, they never change. When data is
// overwritten, a new slice with new blocks is created.
type BlockRef struct {
	// ID is the block's unique identifier (UUID).
	// In block store: stored as blocks/{ID}
	ID string

	// Size is the actual size of this block (<= BlockSize).
	// The last block of a slice may be smaller than BlockSize.
	Size uint32
}

// FileChunkMeta stores per-file chunk configuration.
//
// This metadata is created when a file is first written and is immutable
// for the lifetime of the file.
//
// Storage in BadgerDB:
//
//	Key: chunkmeta:{file_handle}
//	Value: Serialized FileChunkMeta
type FileChunkMeta struct {
	// FileHandle references the file.
	FileHandle []byte

	// BlockSize is the block size for this file (immutable after creation).
	// Defaults to DefaultBlockSize but can be configured per-share.
	BlockSize uint32

	// TotalSize is the current file size in bytes.
	// Updated on writes and truncate.
	TotalSize uint64

	// CreatedAt is when this file's chunk metadata was created.
	CreatedAt time.Time

	// ModifiedAt is when this file was last modified.
	ModifiedAt time.Time
}

// ============================================================================
// Helper Functions
// ============================================================================

// ChunkIndexForOffset calculates the chunk index for a file offset.
func ChunkIndexForOffset(offset uint64) uint32 {
	return uint32(offset / ChunkSize)
}

// OffsetWithinChunk calculates the offset within a chunk.
func OffsetWithinChunk(offset uint64) uint32 {
	return uint32(offset % ChunkSize)
}

// ChunkRange calculates the range of chunks that a byte range spans.
// Returns startChunk and endChunk (inclusive).
func ChunkRange(offset, length uint64) (startChunk, endChunk uint32) {
	if length == 0 {
		return ChunkIndexForOffset(offset), ChunkIndexForOffset(offset)
	}
	startChunk = ChunkIndexForOffset(offset)
	endChunk = ChunkIndexForOffset(offset + length - 1)
	return startChunk, endChunk
}

// NewChunkInfo creates a new empty ChunkInfo for a file.
func NewChunkInfo(fileHandle []byte, chunkIndex uint32) *ChunkInfo {
	return &ChunkInfo{
		FileHandle: fileHandle,
		ChunkIndex: chunkIndex,
		Slices:     make([]SliceInfo, 0),
		Version:    1,
	}
}

// NewFileChunkMeta creates new chunk metadata for a file.
func NewFileChunkMeta(fileHandle []byte, blockSize uint32) *FileChunkMeta {
	now := time.Now()
	return &FileChunkMeta{
		FileHandle: fileHandle,
		BlockSize:  blockSize,
		TotalSize:  0,
		CreatedAt:  now,
		ModifiedAt: now,
	}
}

// ============================================================================
// SliceInfo Methods
// ============================================================================

// End returns the exclusive end offset of this slice within the chunk.
func (s *SliceInfo) End() uint32 {
	return s.Offset + s.Length
}

// Overlaps returns true if this slice overlaps with the given range.
func (s *SliceInfo) Overlaps(offset, length uint32) bool {
	rangeEnd := offset + length
	return s.Offset < rangeEnd && s.End() > offset
}

// Contains returns true if this slice fully contains the given range.
func (s *SliceInfo) Contains(offset, length uint32) bool {
	return s.Offset <= offset && s.End() >= offset+length
}

// ============================================================================
// ChunkInfo Methods
// ============================================================================

// AddSlice prepends a new slice (newest first ordering).
func (c *ChunkInfo) AddSlice(slice SliceInfo) {
	c.Slices = append([]SliceInfo{slice}, c.Slices...)
	c.Version++
}

// SliceCount returns the number of slices in this chunk.
func (c *ChunkInfo) SliceCount() int {
	return len(c.Slices)
}

// NeedsCompaction returns true if this chunk has too many slices.
func (c *ChunkInfo) NeedsCompaction(threshold int) bool {
	return len(c.Slices) > threshold
}

// MaxExtent returns the highest byte offset covered by any slice.
func (c *ChunkInfo) MaxExtent() uint32 {
	var max uint32
	for _, slice := range c.Slices {
		if end := slice.End(); end > max {
			max = end
		}
	}
	return max
}
