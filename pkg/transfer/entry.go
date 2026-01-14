// Package transfer implements background upload for cache-to-block-store persistence.
//
// The transfer package is responsible for:
//   - Eager upload: Upload 4MB blocks as soon as they're ready (don't wait for COMMIT)
//   - Flush: Wait for in-flight uploads and flush remaining partial blocks on COMMIT/CLOSE
//   - Download: Fetch blocks from block store on cache miss, cache them for future reads
//
// Key Design Principles:
//   - Maximize bandwidth: Upload blocks as soon as 4MB is available
//   - Parallel I/O: Upload/download multiple blocks concurrently
//   - Protocol agnostic: Works with both NFS COMMIT and SMB CLOSE
//   - Share-aware keys: Block keys include share name for multi-tenant support
package transfer

import (
	"context"
)

// TransferQueueEntry defines the interface for items that can be queued for transfer.
// This enables different implementations for different storage backends (S3, filesystem, etc.).
type TransferQueueEntry interface {
	// ShareName returns the share name for this entry.
	ShareName() string

	// FileHandle returns the file handle for this entry.
	FileHandle() string

	// PayloadID returns the content ID for block key generation.
	PayloadID() string

	// Execute performs the actual transfer operation.
	// The manager is provided to access the cache and block store.
	Execute(ctx context.Context, manager *TransferManager) error

	// Priority returns the priority of this entry (higher = more important).
	// Used for queue ordering when multiple entries are pending.
	Priority() int
}

// DefaultEntry is the standard implementation of TransferQueueEntry.
// It flushes cache data to the block store.
type DefaultEntry struct {
	shareName  string
	fileHandle string
	payloadID  string
	priority   int
}

// NewDefaultEntry creates a new default transfer entry.
func NewDefaultEntry(shareName string, fileHandle string, payloadID string) *DefaultEntry {
	return &DefaultEntry{
		shareName:  shareName,
		fileHandle: fileHandle,
		payloadID:  payloadID,
		priority:   0,
	}
}

// ShareName returns the share name.
func (e *DefaultEntry) ShareName() string {
	return e.shareName
}

// FileHandle returns the file handle.
func (e *DefaultEntry) FileHandle() string {
	return e.fileHandle
}

// PayloadID returns the content ID.
func (e *DefaultEntry) PayloadID() string {
	return e.payloadID
}

// Execute performs the transfer by flushing remaining cache data.
func (e *DefaultEntry) Execute(ctx context.Context, manager *TransferManager) error {
	return manager.flushRemainingSyncInternal(ctx, e.shareName, e.fileHandle, e.payloadID, true)
}

// Priority returns the entry priority.
func (e *DefaultEntry) Priority() int {
	return e.priority
}

// WithPriority returns a copy of the entry with the specified priority.
func (e *DefaultEntry) WithPriority(priority int) *DefaultEntry {
	return &DefaultEntry{
		shareName:  e.shareName,
		fileHandle: e.fileHandle,
		payloadID:  e.payloadID,
		priority:   priority,
	}
}

// Ensure DefaultEntry implements TransferQueueEntry.
var _ TransferQueueEntry = (*DefaultEntry)(nil)
