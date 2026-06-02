package handlers

import (
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// AsyncPipeReadCallback delivers the final async READ response for a pending
// named-pipe read. data is the raw pipe bytes (nil on cancellation/error).
type AsyncPipeReadCallback func(sessionID, messageID, asyncId uint64, status types.Status, data []byte) error

// PendingPipeRead tracks a named-pipe READ that is waiting for data.
// Created when handlePipeRead finds no data available and returns STATUS_PENDING.
type PendingPipeRead struct {
	FileID    [16]byte
	SessionID uint64
	MessageID uint64
	AsyncId   uint64
	MaxLen    int
	Callback  AsyncPipeReadCallback
}

// PipeReadRegistry tracks outstanding async pipe READ operations.
// Each named-pipe handle can have at most one pending read at a time.
// Thread-safe; all methods acquire the mutex.
type PipeReadRegistry struct {
	mu          sync.Mutex
	byFileID    map[[16]byte]*PendingPipeRead
	byMessageID map[uint64]*PendingPipeRead
	byAsyncId   map[uint64]*PendingPipeRead
}

// NewPipeReadRegistry creates an empty registry.
func NewPipeReadRegistry() *PipeReadRegistry {
	return &PipeReadRegistry{
		byFileID:    make(map[[16]byte]*PendingPipeRead),
		byMessageID: make(map[uint64]*PendingPipeRead),
		byAsyncId:   make(map[uint64]*PendingPipeRead),
	}
}

// Register adds a pending pipe read. If another pending read already exists
// for the same FileID, it is replaced (the old entry is completed first via
// its callback with STATUS_CANCELLED to avoid leaking async slots).
func (r *PipeReadRegistry) Register(p *PendingPipeRead) {
	r.mu.Lock()
	var displaced *PendingPipeRead
	if old, ok := r.byFileID[p.FileID]; ok {
		displaced = old
		delete(r.byMessageID, old.MessageID)
		delete(r.byAsyncId, old.AsyncId)
		logger.Warn("PipeReadRegistry: replacing existing pending read",
			"fileID", p.FileID,
			"oldAsyncId", old.AsyncId)
	}
	r.byFileID[p.FileID] = p
	r.byMessageID[p.MessageID] = p
	r.byAsyncId[p.AsyncId] = p
	r.mu.Unlock()

	if displaced != nil && displaced.Callback != nil {
		go func(pr *PendingPipeRead) {
			if err := pr.Callback(pr.SessionID, pr.MessageID, pr.AsyncId, types.StatusCancelled, nil); err != nil {
				logger.Warn("PipeReadRegistry: failed to cancel displaced READ", "asyncId", pr.AsyncId, "error", err)
			}
		}(displaced)
	}
}

// removeLocked deletes p from all three indexes. Caller must hold r.mu.
func (r *PipeReadRegistry) removeLocked(p *PendingPipeRead) {
	delete(r.byFileID, p.FileID)
	delete(r.byMessageID, p.MessageID)
	delete(r.byAsyncId, p.AsyncId)
}

// UnregisterByFileID removes and returns the pending read for the given FileID.
// Returns nil if none is registered.
func (r *PipeReadRegistry) UnregisterByFileID(fileID [16]byte) *PendingPipeRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byFileID[fileID]
	if !ok {
		return nil
	}
	r.removeLocked(p)
	return p
}

// UnregisterByMessageID removes and returns the pending read with the given MessageID.
// Returns nil if none is registered.
func (r *PipeReadRegistry) UnregisterByMessageID(messageID uint64) *PendingPipeRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byMessageID[messageID]
	if !ok {
		return nil
	}
	r.removeLocked(p)
	return p
}

// UnregisterByAsyncId removes and returns the pending read with the given AsyncId.
// Returns nil if none is registered.
func (r *PipeReadRegistry) UnregisterByAsyncId(asyncId uint64) *PendingPipeRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byAsyncId[asyncId]
	if !ok {
		return nil
	}
	r.removeLocked(p)
	return p
}

// UnregisterAllForSession removes and returns all pending reads for the given session.
func (r *PipeReadRegistry) UnregisterAllForSession(sessionID uint64) []*PendingPipeRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []*PendingPipeRead
	for _, p := range r.byFileID {
		if p.SessionID != sessionID {
			continue
		}
		r.removeLocked(p)
		result = append(result, p)
	}
	return result
}
