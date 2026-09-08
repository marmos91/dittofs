package handlers

import (
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
	// ConnID is the connection the READ arrived on; both cancel keys are
	// scoped to it.
	ConnID    uint64
	MessageID uint64
	AsyncId   uint64
	MaxLen    int
	Callback  AsyncPipeReadCallback
}

// Secondary-index slots for PipeReadRegistry, in configured order.
const (
	pipeIdxFileID = iota
	pipeIdxMsgKey
)

// pipeMsgKey identifies a pending pipe READ by the connection it arrived on
// plus its MessageID, which is only unique within that connection.
type pipeMsgKey struct {
	ConnID    uint64
	MessageID uint64
}

// PipeReadRegistry tracks outstanding async pipe READ operations.
// Each named-pipe handle can have at most one pending read at a time.
// Thread-safe; all methods acquire the mutex.
type PipeReadRegistry struct {
	reg *pendingRegistry[PendingPipeRead]
}

// NewPipeReadRegistry creates an empty registry.
func NewPipeReadRegistry() *PipeReadRegistry {
	return &PipeReadRegistry{
		reg: newPendingRegistry(registryConfig[PendingPipeRead]{
			asyncID: func(p *PendingPipeRead) uint64 { return p.AsyncId },
			connID:  func(p *PendingPipeRead) uint64 { return p.ConnID },
			indexes: []keyFunc[PendingPipeRead]{
				func(p *PendingPipeRead) any { return p.FileID },
				func(p *PendingPipeRead) any {
					return pipeMsgKey{ConnID: p.ConnID, MessageID: p.MessageID}
				},
			},
		}),
	}
}

// Register adds a pending pipe read. If another pending read already exists
// for the same FileID, it is replaced (the old entry is completed first via
// its callback with STATUS_CANCELLED to avoid leaking async slots).
func (r *PipeReadRegistry) Register(p *PendingPipeRead) {
	r.reg.mu.Lock()
	var displaced *PendingPipeRead
	if old := r.reg.lookupLocked(pipeIdxFileID, p.FileID); old != nil {
		displaced = old
		r.reg.removeLocked(old.AsyncId)
		logger.Warn("PipeReadRegistry: replacing existing pending read",
			"fileID", p.FileID,
			"oldAsyncId", old.AsyncId)
	}
	r.reg.insertLocked(p)
	r.reg.mu.Unlock()

	if displaced != nil && displaced.Callback != nil {
		go func(pr *PendingPipeRead) {
			if err := pr.Callback(pr.SessionID, pr.MessageID, pr.AsyncId, types.StatusCancelled, nil); err != nil {
				logger.Warn("PipeReadRegistry: failed to cancel displaced READ", "asyncId", pr.AsyncId, "error", err)
			}
		}(displaced)
	}
}

// UnregisterByFileID removes and returns the pending read for the given FileID.
// Returns nil if none is registered.
func (r *PipeReadRegistry) UnregisterByFileID(fileID [16]byte) *PendingPipeRead {
	return r.reg.unregisterByIndex(pipeIdxFileID, fileID)
}

// UnregisterByMessageID removes and returns the pending read matching
// (connID, messageID). Returns nil if none is registered.
func (r *PipeReadRegistry) UnregisterByMessageID(connID, messageID uint64) *PendingPipeRead {
	return r.reg.unregisterByIndex(pipeIdxMsgKey, pipeMsgKey{ConnID: connID, MessageID: messageID})
}

// UnregisterByAsyncId removes and returns the pending read parked on
// (connID, asyncId). Returns nil if none is registered.
func (r *PipeReadRegistry) UnregisterByAsyncId(connID, asyncId uint64) *PendingPipeRead {
	return r.reg.unregisterByAsyncIDOn(asyncId, connID)
}

// UnregisterAllForSession removes and returns all pending reads for the given session.
func (r *PipeReadRegistry) UnregisterAllForSession(sessionID uint64) []*PendingPipeRead {
	return r.reg.unregisterMatching(func(p *PendingPipeRead) bool {
		return p.SessionID == sessionID
	})
}
