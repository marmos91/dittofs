// Package transfer implements background transfer for cache-to-block-store operations.
//
// The transfer package is responsible for:
//   - Eager upload: Upload 4MB blocks as soon as they're ready (don't wait for COMMIT)
//   - Download: Fetch blocks from block store on cache miss, with download priority
//   - Prefetch: Speculatively fetch upcoming blocks for sequential reads
//   - Flush: Wait for in-flight operations and flush remaining partial blocks on COMMIT/CLOSE
//
// Key Design Principles:
//   - Unified queue: All transfers (upload, download, prefetch) use single worker pool
//   - Priority scheduling: Downloads > Uploads > Prefetch
//   - Parallel I/O: Upload/download multiple blocks concurrently
//   - Protocol agnostic: Works with both NFS COMMIT and SMB CLOSE
//   - In-flight deduplication: Avoid duplicate downloads for same block
package transfer

import "fmt"

// TransferRequest holds data for a pending transfer operation.
//
// All transfer requests specify block coordinates (payloadID + chunkIdx + blockIdx).
// Downloads and prefetch fetch blocks from block store to cache.
// Uploads push blocks from cache to block store.
type TransferRequest struct {
	// Type determines the transfer type and priority.
	Type TransferType

	// PayloadID is the content ID (used for cache key and block keys).
	// This is the sole identifier for file content.
	PayloadID string

	// ChunkIdx is the chunk index (for block-level operations).
	ChunkIdx uint32

	// BlockIdx is the block index within the chunk.
	BlockIdx uint32

	// Offset is the offset within the chunk (for partial reads).
	Offset uint32

	// Length is the data length (for partial reads).
	Length uint32

	// Done channel for synchronous operations. Nil for async (fire-and-forget).
	// Caller blocks on this channel until the operation completes.
	Done chan error
}

// NewDownloadRequest creates a download request for a specific block.
func NewDownloadRequest(payloadID string, chunkIdx, blockIdx uint32, done chan error) TransferRequest {
	return TransferRequest{
		Type:      TransferDownload,
		PayloadID: payloadID,
		ChunkIdx:  chunkIdx,
		BlockIdx:  blockIdx,
		Done:      done,
	}
}

// NewPrefetchRequest creates a prefetch request for a specific block (best-effort).
func NewPrefetchRequest(payloadID string, chunkIdx, blockIdx uint32) TransferRequest {
	return TransferRequest{
		Type:      TransferPrefetch,
		PayloadID: payloadID,
		ChunkIdx:  chunkIdx,
		BlockIdx:  blockIdx,
		Done:      nil, // Prefetch is always async
	}
}

// NewBlockUploadRequest creates an upload request for a specific block.
func NewBlockUploadRequest(payloadID string, chunkIdx, blockIdx uint32) TransferRequest {
	return TransferRequest{
		Type:      TransferUpload,
		PayloadID: payloadID,
		ChunkIdx:  chunkIdx,
		BlockIdx:  blockIdx,
		Done:      nil, // Eager uploads are always async
	}
}

// FormatBlockKey returns the block store key for a block.
// Format: {payloadID}/chunk-{chunkIdx}/block-{blockIdx}
// Note: payloadID already includes the share name (e.g., "export/path/to/file")
func FormatBlockKey(payloadID string, chunkIdx, blockIdx uint32) string {
	return fmt.Sprintf("%s/chunk-%d/block-%d", payloadID, chunkIdx, blockIdx)
}

// BlockKey returns a unique string key for this block.
func (r TransferRequest) BlockKey() string {
	return FormatBlockKey(r.PayloadID, r.ChunkIdx, r.BlockIdx)
}

// WithPriority returns a copy of the request with the specified type (for priority).
func (r TransferRequest) WithPriority(t TransferType) TransferRequest {
	r.Type = t
	return r
}
