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
}

// PendingWritesTracker manages uncommitted write metadata.
// This is used to batch metadata commits for better performance.
type PendingWritesTracker struct {
	mu      sync.RWMutex
	pending map[string]*PendingWriteState // handleKey -> state

	// Stable, bounded stripes serialize each flush's read -> metadata commit ->
	// finish, and the pops that discard pending state.
	// Removing a lock when its pending entry is popped would let a new flush
	// overtake the still-running commit that owns the old lock.
	flushLocks [256]sync.Mutex
}

// NewPendingWritesTracker creates a new tracker.
func NewPendingWritesTracker() *PendingWritesTracker {
	return &PendingWritesTracker{pending: make(map[string]*PendingWriteState)}
}

// GetFlushLock returns a stable lock for a handle's metadata flush. Unrelated
// handles can share a stripe; the fixed bank bounds memory without retiring
// a mutex that a flush or its waiters still own.
func (t *PendingWritesTracker) GetFlushLock(handle FileHandle) *sync.Mutex {
	var h uint32 = 2166136261
	for _, b := range []byte(pendingWriteKey(handle)) {
		h ^= uint32(b)
		h *= 16777619
	}
	return &t.flushLocks[h%uint32(len(t.flushLocks))]
}

// pendingWriteKey unifies the handle spellings accepted by metadata stores.
// Deferred writes, flush exclusion and cache invalidation must all address the
// same inode. Invalid handles retain their opaque key; validation belongs to
// the service entrypoints.
func pendingWriteKey(handle FileHandle) string {
	share, id, err := DecodeFileHandle(handle)
	if err != nil {
		return string(handle)
	}
	return share + ":" + id.String()
}

// handleKey converts a FileHandle to a map key.
func handleKey(handle FileHandle) string {
	return string(handle)
}

// RecordWrite records a pending write for deferred commit.
// Returns the current pending state (with max size across all writes).
func (t *PendingWritesTracker) RecordWrite(handle FileHandle, intent *WriteOperation, clearSetuid bool) *PendingWriteState {
	key := pendingWriteKey(handle)

	t.mu.Lock()
	defer t.mu.Unlock()

	state, exists := t.pending[key]
	if !exists {
		// First write to this file
		state = &PendingWriteState{
			MaxSize:           intent.NewSize,
			LastMtime:         intent.NewMtime,
			PayloadID:         intent.PayloadID,
			PreWriteAttr:      intent.PreWriteAttr,
			ClearSetuidSetgid: clearSetuid,
		}
		t.pending[key] = state
	} else {
		// Update existing pending state
		if intent.NewSize > state.MaxSize {
			state.MaxSize = intent.NewSize
		}
		// Set LastMtime only on the first actual write (state may have been
		// created by SetCachedFile with zero mtime). Once set, freeze it —
		// this ensures all WRITE responses return the same mtime, which is
		// critical for NFS client page cache stability. The Linux NFS client
		// keys its cache on (mtime, ctime, size) and invalidates it if mtime
		// changes between WRITEs.
		//
		// ponytail: this state is keyed by file handle alone, with no notion of
		// which client wrote, so the freeze applies to every client at once.
		// The stable mtime/ctime it buys one writer also holds the NFSv4 change
		// attribute still, since that attribute is encoded from ctime. So from
		// the second write of a session onward, another client's writes to the
		// same file go unannounced: MaxSize is not frozen, so a write that
		// extends the file moves size, but one that overwrites in place moves
		// no attribute at all, and a reader re-reading the change attribute
		// sees the frozen value and keeps serving its cached copy. Upgrade to a
		// per-client freeze — the writing client sees its own stable value
		// while other clients observe the real one — if concurrent writers to
		// a single file become a case to serve.
		if state.LastMtime.IsZero() {
			state.LastMtime = intent.NewMtime
		}
		if clearSetuid {
			state.ClearSetuidSetgid = true
		}
		// A state created by SetCachedFile carries no payload, so the first real
		// write fills in the one the flush needs to locate this file's bytes.
		if state.PayloadID == "" {
			state.PayloadID = intent.PayloadID
		}
	}

	return state
}

// GetPendingSize returns the pending size for a file, if any.
// Returns (size, true) if there's a pending write, (0, false) otherwise.
func (t *PendingWritesTracker) GetPendingSize(handle FileHandle) (uint64, bool) {
	key := pendingWriteKey(handle)

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
	key := pendingWriteKey(handle)

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
	key := pendingWriteKey(handle)

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
	key := pendingWriteKey(handle)

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
	key := pendingWriteKey(handle)

	t.mu.Lock()
	defer t.mu.Unlock()

	state, exists := t.pending[key]
	if !exists {
		return nil, false
	}
	delete(t.pending, key)

	return state, true
}

// FinishFlush drops from a handle's pending state what a flush of flushed has
// committed: its mtime, its setuid/setgid clearing, and the size up to
// committedSize. A write recorded after the flush copied the state may have
// moved it on, and whatever that write moved stays for the next flush. With
// nothing left to commit, the entry only caches the file again.
func (t *PendingWritesTracker) FinishFlush(handle FileHandle, flushed *PendingWriteState, committedSize uint64) {
	key := pendingWriteKey(handle)

	t.mu.Lock()
	defer t.mu.Unlock()

	cur, exists := t.pending[key]
	if !exists {
		return
	}
	if cur.MaxSize <= committedSize {
		cur.MaxSize = 0
	}
	// A later write reuses the frozen mtime, so an equal one is the one just
	// committed; any other was set since and has yet to reach the store.
	if cur.LastMtime.Equal(flushed.LastMtime) {
		cur.LastMtime = time.Time{}
	}
	if flushed.ClearSetuidSetgid {
		cur.ClearSetuidSetgid = false
	}
	if cur.MaxSize > 0 || !cur.LastMtime.IsZero() || cur.ClearSetuidSetgid {
		return
	}
	if cur.CachedFile == nil {
		delete(t.pending, key)
		return
	}
	t.pending[key] = &PendingWriteState{CachedFile: cur.CachedFile}
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

// PendingHandles snapshots the handles without taking their pending state.
// Shutdown must acquire each handle's flush lock before popping its state,
// just like an ordinary flush, so clone reconciliation cannot miss it.
func (t *PendingWritesTracker) PendingHandles() []FileHandle {
	t.mu.RLock()
	defer t.mu.RUnlock()
	handles := make([]FileHandle, 0, len(t.pending))
	for key := range t.pending {
		handles = append(handles, FileHandle(key))
	}
	return handles
}

// Count returns the number of pending writes.
func (t *PendingWritesTracker) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.pending)
}

// UpdatePendingMtime updates the LastMtime in the pending state for a handle.
// This is used by frozen timestamp support: when a SET_INFO(-1) freezes Mtime,
// the pending state's LastMtime must be updated to the frozen value so that
// MetadataService.GetFile() merge returns the frozen value instead of the
// original write timestamp.
// Returns true if the pending state was updated, false if no pending state exists.
func (t *PendingWritesTracker) UpdatePendingMtime(handle FileHandle, mtime time.Time) bool {
	key := pendingWriteKey(handle)

	t.mu.Lock()
	defer t.mu.Unlock()

	state, exists := t.pending[key]
	if !exists {
		return false
	}
	state.LastMtime = mtime
	return true
}

// InvalidateCache removes the cached file metadata for a handle.
// This should be called when file attributes change (e.g., via SETATTR)
// to ensure subsequent writes use fresh attributes from the store.
func (t *PendingWritesTracker) InvalidateCache(handle FileHandle) {
	key := pendingWriteKey(handle)

	t.mu.Lock()
	defer t.mu.Unlock()

	state, exists := t.pending[key]
	if !exists {
		return
	}

	// Clear the cached file, keeping other pending state if present
	state.CachedFile = nil

	// If there's no other pending state, remove the entry entirely
	if state.PreWriteAttr == nil && state.MaxSize == 0 {
		delete(t.pending, key)
	}
}
