package wal

import (
	"errors"
)

// Persister errors
var (
	// ErrPersisterClosed is returned when operations are attempted on a closed persister.
	ErrPersisterClosed = errors.New("persister is closed")

	// ErrCorrupted is returned when the WAL file is corrupted.
	ErrCorrupted = errors.New("WAL file corrupted")

	// ErrVersionMismatch is returned when the WAL file version doesn't match.
	ErrVersionMismatch = errors.New("WAL file version mismatch")
)

// Persister defines the interface for WAL persistence.
//
// The cache uses this interface to persist operations for crash recovery.
// Implementations can use different storage backends (mmap, file-based, etc).
//
// Thread Safety:
// Implementations must be safe for concurrent use from multiple goroutines.
type Persister interface {
	// AppendSlice appends a slice entry to the WAL.
	// This is called for every new slice written to the cache.
	AppendSlice(entry *SliceEntry) error

	// AppendRemove appends a file removal entry to the WAL.
	// This is called when a file is removed from the cache.
	AppendRemove(fileHandle []byte) error

	// Sync forces pending writes to durable storage.
	// Uses async semantics - the OS handles actual disk flush.
	Sync() error

	// Recover replays the WAL and returns all recovered slice entries.
	// Called on startup to reconstruct in-memory state.
	// Returns slice entries grouped by file handle.
	Recover() ([]SliceEntry, error)

	// Close releases resources held by the persister.
	// Syncs pending data before closing.
	Close() error

	// IsEnabled returns true if persistence is enabled.
	IsEnabled() bool
}

// NullPersister is a no-op implementation for when persistence is disabled.
// It implements all Persister methods but does nothing.
type NullPersister struct{}

// NewNullPersister creates a new no-op persister.
func NewNullPersister() *NullPersister {
	return &NullPersister{}
}

// AppendSlice is a no-op.
func (p *NullPersister) AppendSlice(entry *SliceEntry) error {
	return nil
}

// AppendRemove is a no-op.
func (p *NullPersister) AppendRemove(fileHandle []byte) error {
	return nil
}

// Sync is a no-op.
func (p *NullPersister) Sync() error {
	return nil
}

// Recover returns an empty slice (nothing to recover).
func (p *NullPersister) Recover() ([]SliceEntry, error) {
	return nil, nil
}

// Close is a no-op.
func (p *NullPersister) Close() error {
	return nil
}

// IsEnabled returns false (persistence disabled).
func (p *NullPersister) IsEnabled() bool {
	return false
}

// Ensure NullPersister implements Persister.
var _ Persister = (*NullPersister)(nil)
