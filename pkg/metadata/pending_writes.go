package metadata

import (
	"sync"
	"time"
)

// PendingWriteState tracks uncommitted metadata changes for a file.
// This enables batching multiple writes into a single metadata commit.
type PendingWriteState struct {
	// MaxSize is the maximum size seen from all pending writes
	MaxSize uint64

	// LastMtime is the most recent modification time
	LastMtime time.Time

	// PayloadID for the file (needed for commit)
	PayloadID PayloadID

	// PreWriteAttr from the first write (for WCC data)
	PreWriteAttr *FileAttr

	// ClearSetuidSetgid if any write was from non-root
	ClearSetuidSetgid bool

	// CachedFile stores validated file metadata for fast-path PrepareWrite
	// This avoids repeated GetFile calls for sequential writes
	CachedFile *File

	// IsCOW indicates this write triggered copy-on-write
	IsCOW bool

	// COWSourcePayloadID is the source for lazy copy (only set when IsCOW is true)
	COWSourcePayloadID PayloadID
}

// PendingWritesTracker manages uncommitted write metadata.
// This is used to batch metadata commits for better performance.
type PendingWritesTracker struct {
	mu      sync.RWMutex
	pending map[string]*PendingWriteState // handleKey -> state
}

// NewPendingWritesTracker creates a new tracker.
func NewPendingWritesTracker() *PendingWritesTracker {
	return &PendingWritesTracker{
		pending: make(map[string]*PendingWriteState),
	}
}

// handleKey converts a FileHandle to a map key.
func handleKey(handle FileHandle) string {
	return string(handle)
}

// RecordWrite records a pending write for deferred commit.
// Returns the current pending state (with max size across all writes).
func (t *PendingWritesTracker) RecordWrite(handle FileHandle, intent *WriteOperation, clearSetuid bool) *PendingWriteState {
	key := handleKey(handle)

	t.mu.Lock()
	defer t.mu.Unlock()

	state, exists := t.pending[key]
	if !exists {
		// First write to this file
		state = &PendingWriteState{
			MaxSize:            intent.NewSize,
			LastMtime:          intent.NewMtime,
			PayloadID:          intent.PayloadID,
			PreWriteAttr:       intent.PreWriteAttr,
			ClearSetuidSetgid:  clearSetuid,
			IsCOW:              intent.IsCOW,
			COWSourcePayloadID: intent.COWSourcePayloadID,
		}
		t.pending[key] = state
	} else {
		// Update existing pending state
		if intent.NewSize > state.MaxSize {
			state.MaxSize = intent.NewSize
		}
		state.LastMtime = intent.NewMtime
		if clearSetuid {
			state.ClearSetuidSetgid = true
		}
		// COW is set on first write and doesn't change for subsequent writes
		// (once COW is triggered, we're writing to the new PayloadID)
	}

	return state
}

// GetPendingSize returns the pending size for a file, if any.
// Returns (size, true) if there's a pending write, (0, false) otherwise.
func (t *PendingWritesTracker) GetPendingSize(handle FileHandle) (uint64, bool) {
	key := handleKey(handle)

	t.mu.RLock()
	defer t.mu.RUnlock()

	state, exists := t.pending[key]
	if !exists {
		return 0, false
	}
	return state.MaxSize, true
}

// GetPending returns the pending state for a file, if any.
func (t *PendingWritesTracker) GetPending(handle FileHandle) (*PendingWriteState, bool) {
	key := handleKey(handle)

	t.mu.RLock()
	defer t.mu.RUnlock()

	state, exists := t.pending[key]
	if !exists {
		return nil, false
	}
	// Return a copy to avoid races
	stateCopy := *state
	return &stateCopy, true
}

// GetCachedFile returns the cached file metadata for fast-path PrepareWrite.
// Returns nil if no cache exists for this file.
func (t *PendingWritesTracker) GetCachedFile(handle FileHandle) *File {
	key := handleKey(handle)

	t.mu.RLock()
	defer t.mu.RUnlock()

	state, exists := t.pending[key]
	if !exists || state.CachedFile == nil {
		return nil
	}
	// Return a copy to avoid races
	fileCopy := *state.CachedFile
	return &fileCopy
}

// SetCachedFile stores file metadata for fast-path PrepareWrite.
func (t *PendingWritesTracker) SetCachedFile(handle FileHandle, file *File) {
	key := handleKey(handle)

	t.mu.Lock()
	defer t.mu.Unlock()

	state, exists := t.pending[key]
	if !exists {
		// Create a new pending state just for caching
		t.pending[key] = &PendingWriteState{
			CachedFile: file,
		}
	} else {
		state.CachedFile = file
	}
}

// PopPending removes and returns the pending state for a file.
// Used when flushing pending writes to the store.
func (t *PendingWritesTracker) PopPending(handle FileHandle) (*PendingWriteState, bool) {
	key := handleKey(handle)

	t.mu.Lock()
	defer t.mu.Unlock()

	state, exists := t.pending[key]
	if !exists {
		return nil, false
	}
	delete(t.pending, key)
	return state, true
}

// PendingEntry pairs a handle with its pending state.
type PendingEntry struct {
	Handle FileHandle
	State  *PendingWriteState
}

// PopAllPending removes and returns all pending states.
// Used for bulk flush operations.
func (t *PendingWritesTracker) PopAllPending() []PendingEntry {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make([]PendingEntry, 0, len(t.pending))
	for key, state := range t.pending {
		result = append(result, PendingEntry{
			Handle: FileHandle(key),
			State:  state,
		})
	}
	t.pending = make(map[string]*PendingWriteState)
	return result
}

// Count returns the number of pending writes.
func (t *PendingWritesTracker) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.pending)
}
