// Package state provides NFSv4 break handler for cross-protocol delegation recall.
//
// NFSBreakHandler implements lock.BreakCallbacks and translates LockManager
// delegation recalls into NFS CB_RECALL messages sent via the backchannel.
package state

import (
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// NFSBreakHandler implements lock.BreakCallbacks for NFS delegation recall.
// When the shared LockManager dispatches a delegation recall, this handler
// looks up the NFS stateid and sends CB_RECALL via the backchannel.
// CB_RECALL is sent asynchronously to avoid blocking the callback.
type NFSBreakHandler struct {
	stateManager *StateManager
}

// NewNFSBreakHandler creates a new NFSBreakHandler.
func NewNFSBreakHandler(sm *StateManager) *NFSBreakHandler {
	return &NFSBreakHandler{
		stateManager: sm,
	}
}

// OnDelegationRecall looks up the NFS stateid for the delegation and sends
// CB_RECALL via the backchannel. No-op if no NFS stateid mapping exists.
func (h *NFSBreakHandler) OnDelegationRecall(handleKey string, ul *lock.UnifiedLock) {
	if ul == nil || ul.Delegation == nil {
		return
	}

	delegID := ul.Delegation.DelegationID

	// Single lock section: look up stateid via O(1) map, find DelegationState,
	// and mark RecallSent atomically to avoid races with concurrent returns.
	h.stateManager.mu.Lock()
	stateid, found := h.stateManager.delegStateidMap[delegID]
	if !found {
		h.stateManager.mu.Unlock()
		logger.Debug("NFSBreakHandler: no NFS stateid for delegation, skipping",
			"delegationID", delegID,
			"handleKey", handleKey)
		return
	}

	// Use the stateid.Other to look up the DelegationState in O(1).
	deleg, exists := h.stateManager.delegByOther[stateid.Other]
	if !exists || deleg.LockManagerDelegID != delegID {
		h.stateManager.mu.Unlock()
		logger.Debug("NFSBreakHandler: delegation state not found",
			"delegationID", delegID)
		return
	}

	deleg.RecallSent = true
	deleg.RecallTime = time.Now()
	h.stateManager.mu.Unlock()

	logger.Debug("NFSBreakHandler: dispatching CB_RECALL",
		"delegationID", delegID,
		"clientID", deleg.ClientID,
		"handleKey", handleKey)

	go h.stateManager.sendRecall(deleg)
}

// OnOpLockBreak is a no-op for NFS (no SMB-style leases).
func (h *NFSBreakHandler) OnOpLockBreak(_ string, _ *lock.UnifiedLock, _ uint32) {}

// OnByteRangeRevoke is a no-op for NFS.
func (h *NFSBreakHandler) OnByteRangeRevoke(_ string, _ *lock.UnifiedLock, _ string) {}

// OnAccessConflict is a no-op for NFS.
func (h *NFSBreakHandler) OnAccessConflict(_ string, _ *lock.UnifiedLock, _ lock.AccessMode) {}

// Compile-time verification that NFSBreakHandler implements BreakCallbacks.
var _ lock.BreakCallbacks = (*NFSBreakHandler)(nil)
