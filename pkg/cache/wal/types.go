// Package wal provides write-ahead logging for cache persistence.
//
// The WAL (Write-Ahead Log) ensures crash recovery for cached data.
// It uses an append-only log format where operations are recorded
// before being applied, allowing reconstruction of state on restart.
package wal

import (
	"time"
)

// SliceState represents the state of a slice in the cache.
type SliceState int

const (
	// SliceStatePending indicates the slice has unflushed data.
	SliceStatePending SliceState = iota

	// SliceStateFlushed indicates the slice has been persisted to block storage.
	SliceStateFlushed

	// SliceStateUploading indicates the slice is currently being uploaded.
	SliceStateUploading
)

// String returns the string representation of SliceState.
func (s SliceState) String() string {
	switch s {
	case SliceStatePending:
		return "Pending"
	case SliceStateFlushed:
		return "Flushed"
	case SliceStateUploading:
		return "Uploading"
	default:
		return "Unknown"
	}
}

// BlockRef references an immutable block in the block store.
type BlockRef struct {
	// ID is the block's unique identifier in the block store.
	ID string

	// Size is the actual size of this block (may be < BlockSize for last block).
	Size uint32
}

// Slice represents a slice of data within a chunk.
// This is the canonical slice type used by both cache and WAL.
type Slice struct {
	// ID uniquely identifies this slice.
	ID string

	// Offset is the byte offset within the chunk (0 to ChunkSize-1).
	Offset uint32

	// Length is the size of this slice in bytes.
	Length uint32

	// Data contains the actual slice content.
	Data []byte

	// State indicates whether this slice is pending, uploading, or flushed.
	State SliceState

	// CreatedAt is when this slice was created (for newest-wins ordering).
	CreatedAt time.Time

	// BlockRefs contains references to blocks after flushing.
	BlockRefs []BlockRef
}

// SliceEntry represents a slice entry in the WAL.
// It embeds Slice and adds context fields needed for recovery.
type SliceEntry struct {
	// FileHandle identifies the file this slice belongs to.
	FileHandle string

	// ChunkIdx is the chunk index within the file.
	ChunkIdx uint32

	// Slice is the embedded slice data.
	Slice
}
