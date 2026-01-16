// Package wal provides write-ahead logging for cache persistence.
//
// The WAL (Write-Ahead Log) ensures crash recovery for cached data.
// It uses an append-only log format where operations are recorded
// before being applied, allowing reconstruction of state on restart.
//
// Block-Level WAL Format:
// The WAL records individual block writes with their coordinates:
// (payloadID, chunkIdx, blockIdx, offsetInBlock, data)
// On recovery, writes are replayed into block buffers.
package wal

// BlockWriteEntry represents a single write operation in the WAL.
// Each entry records a write to a specific location within a block.
type BlockWriteEntry struct {
	// PayloadID identifies the file this write belongs to.
	PayloadID string

	// ChunkIdx is the chunk index within the file.
	ChunkIdx uint32

	// BlockIdx is the block index within the chunk.
	BlockIdx uint32

	// OffsetInBlock is the byte offset within the block (0 to BlockSize-1).
	OffsetInBlock uint32

	// Data contains the bytes written.
	Data []byte
}
