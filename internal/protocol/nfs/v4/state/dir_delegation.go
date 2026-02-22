// Package state -- directory delegation management for NFSv4.1.
//
// Directory delegations (RFC 8881 Section 18.39) allow a client to cache
// directory contents and receive change notifications via CB_NOTIFY instead
// of re-reading via READDIR.
//
// Key design:
//   - GrantDirDelegation creates directory delegations with limit checking
//   - NotifyDirChange batches notifications per-delegation with a timer
//   - flushDirNotifications drains the batch and sends CB_NOTIFY
//   - Lock ordering: sm.mu before deleg.NotifMu (never reverse)

package state

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// maxBatchSize is the maximum number of notifications per delegation before
// an immediate flush is triggered (count-based flush).
const maxBatchSize = 100

// GrantDirDelegation creates a new directory delegation for a client.
//
// It performs the following checks before granting:
//   - Delegations must be enabled
//   - Client must have a valid lease
//   - Total delegation count must be below maxDelegations limit
//   - No duplicate directory delegation for same client+handle
//
// Returns the DelegationState on success, or nil with an error.
//
// Caller must NOT hold sm.mu (method acquires it).
func (sm *StateManager) GrantDirDelegation(clientID uint64, dirFH []byte, notifMask uint32) (*DelegationState, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Check delegations enabled
	if !sm.delegationsEnabled {
		return nil, fmt.Errorf("delegations disabled")
	}

	// Check client has valid lease (v4.0 or v4.1)
	_, v40Exists := sm.clientsByID[clientID]
	_, v41Exists := sm.v41ClientsByID[clientID]
	if !v40Exists && !v41Exists {
		return nil, &NFS4StateError{
			Status:  types.NFS4ERR_EXPIRED,
			Message: fmt.Sprintf("client %d not found", clientID),
		}
	}

	// Check total delegation count against limit
	if sm.maxDelegations > 0 && len(sm.delegByOther) >= sm.maxDelegations {
		return nil, fmt.Errorf("delegation limit exceeded (%d)", sm.maxDelegations)
	}

	// Check no existing directory delegation for same client+handle
	fhKey := string(dirFH)
	for _, existing := range sm.delegByFile[fhKey] {
		if existing.ClientID == clientID && existing.IsDirectory && !existing.Revoked {
			return nil, fmt.Errorf("duplicate directory delegation for client %d on handle", clientID)
		}
	}

	// Generate stateid (same type byte 0x03 as file delegations)
	other := sm.generateStateidOther(StateTypeDeleg)
	stateid := types.Stateid4{
		Seqid: 1,
		Other: other,
	}

	// Generate random cookie verifier
	var cookieVerf [8]byte
	if _, err := rand.Read(cookieVerf[:]); err != nil {
		// Fallback to time-based if crypto/rand fails
		now := time.Now().UnixNano()
		for i := range 8 {
			cookieVerf[i] = byte(now >> (uint(i) * 8))
		}
	}

	fhCopy := make([]byte, len(dirFH))
	copy(fhCopy, dirFH)

	deleg := &DelegationState{
		Stateid:          stateid,
		ClientID:         clientID,
		FileHandle:       fhCopy,
		DelegType:        types.OPEN_DELEGATE_READ, // directory delegations are read-like
		IsDirectory:      true,
		NotificationMask: notifMask,
		CookieVerf:       cookieVerf,
	}

	sm.delegByOther[other] = deleg
	sm.delegByFile[fhKey] = append(sm.delegByFile[fhKey], deleg)

	logger.Info("Directory delegation granted",
		"client_id", clientID,
		"notification_mask", fmt.Sprintf("0x%x", notifMask),
		"stateid_seqid", stateid.Seqid)

	sm.delegationMetrics.RecordGrant("directory")

	return deleg, nil
}

// NotifyDirChange sends a directory change notification to all clients
// holding directory delegations on the specified directory handle.
//
// Notifications are batched per-delegation using a timer. When the batch
// timer fires (or count exceeds maxBatchSize), accumulated notifications
// are flushed via CB_NOTIFY.
//
// This method is non-blocking: it does NOT hold sm.mu during backchannel sends.
//
// Caller must NOT hold sm.mu (method acquires RLock).
func (sm *StateManager) NotifyDirChange(dirFH []byte, notif DirNotification) {
	sm.mu.RLock()
	delegs := sm.delegByFile[string(dirFH)]
	// Filter eligible delegations while holding the lock to avoid data races
	// on deleg.Revoked and deleg.RecallSent (written under sm.mu.Lock).
	var targets []*DelegationState
	for _, deleg := range delegs {
		if deleg.IsDirectory && !deleg.Revoked && !deleg.RecallSent {
			targets = append(targets, deleg)
		}
	}
	sm.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	for _, deleg := range targets {

		// Conflict-based recall: if the notification comes from a different
		// client than the delegation holder, recall the delegation.
		if notif.OriginClientID != 0 && deleg.ClientID != notif.OriginClientID {
			go sm.RecallDirDelegation(deleg, "conflict")
			continue
		}

		// Check if client subscribed to this notification type
		if deleg.NotificationMask&(1<<notif.Type) == 0 {
			continue
		}

		deleg.NotifMu.Lock()
		deleg.PendingNotifs = append(deleg.PendingNotifs, notif)
		count := len(deleg.PendingNotifs)

		// Start/reset batch timer if not already running
		sm.resetBatchTimer(deleg)

		// Count-based flush: if too many notifications have accumulated
		if count >= maxBatchSize {
			// Stop the timer since we are flushing now
			if deleg.BatchTimer != nil {
				deleg.BatchTimer.Stop()
				deleg.BatchTimer = nil
			}
			deleg.NotifMu.Unlock()
			sm.flushDirNotifications(deleg)
		} else {
			deleg.NotifMu.Unlock()
		}
	}
}

// flushDirNotifications drains pending notifications from a directory
// delegation and sends them via CB_NOTIFY through the backchannel.
//
// The drain pattern: acquire NotifMu, swap PendingNotifs with nil, release
// NotifMu, then encode and send without holding any locks.
func (sm *StateManager) flushDirNotifications(deleg *DelegationState) {
	deleg.NotifMu.Lock()
	pending := deleg.PendingNotifs
	deleg.PendingNotifs = nil
	deleg.NotifMu.Unlock()

	if len(pending) == 0 {
		return
	}

	// Encode CB_NOTIFY payload
	encoded := EncodeCBNotifyOp(&deleg.Stateid, deleg.FileHandle, pending, deleg.NotificationMask)

	// Send via backchannel — only record metric on successful enqueue
	sender := sm.getBackchannelSender(deleg.ClientID)
	if sender != nil {
		req := CallbackRequest{
			OpCode:  types.CB_NOTIFY,
			Payload: encoded,
		}
		if sender.Enqueue(req) {
			sm.delegationMetrics.RecordDirNotification()
		} else {
			logger.Warn("CB_NOTIFY: backchannel queue full, notifications lost",
				"client_id", deleg.ClientID,
				"count", len(pending))
		}
	} else {
		logger.Debug("CB_NOTIFY: no backchannel sender, notifications lost",
			"client_id", deleg.ClientID,
			"count", len(pending))
	}
}

// resetBatchTimer starts a new batch timer if one is not already running.
// Uses time.AfterFunc to trigger flushDirNotifications after the batch window.
//
// Caller must hold deleg.NotifMu.
func (sm *StateManager) resetBatchTimer(deleg *DelegationState) {
	if deleg.BatchTimer != nil {
		// Timer already running -- let it expire naturally
		return
	}

	window := sm.dirDelegBatchWindow
	if window <= 0 {
		window = 50 * time.Millisecond // default
	}

	deleg.BatchTimer = time.AfterFunc(window, func() {
		deleg.NotifMu.Lock()
		deleg.BatchTimer = nil
		deleg.NotifMu.Unlock()

		sm.flushDirNotifications(deleg)
	})
}

// RecallDirDelegation recalls a directory delegation with the given reason.
//
// It flushes any pending notifications before sending the recall (per design:
// client should receive all pending changes before losing the delegation).
//
// For reason="directory_deleted", the delegation is revoked immediately
// (no recall needed since the directory no longer exists).
//
// Caller must NOT hold sm.mu (method acquires Lock).
func (sm *StateManager) RecallDirDelegation(deleg *DelegationState, reason string) {
	sm.delegationMetrics.RecordRecall("directory", reason)

	// For directory_deleted: revoke immediately (no recall)
	if reason == "directory_deleted" {
		sm.mu.Lock()
		deleg.RecallReason = reason
		sm.mu.Unlock()
		sm.RevokeDelegation(deleg.Stateid.Other)
		return
	}

	// Flush pending notifications before recall
	sm.flushDirNotifications(deleg)

	// Mark as recalled and send recall (all fields written under sm.mu)
	sm.mu.Lock()
	deleg.RecallReason = reason
	deleg.RecallSent = true
	deleg.RecallTime = time.Now()
	sm.mu.Unlock()

	// Send recall asynchronously (same pattern as file delegations)
	go sm.sendRecall(deleg)
}

// ShouldRecallDirDelegation checks if any directory delegation holders exist for
// the given directory handle from a DIFFERENT client. If so, sends recall to
// those holders (CB_RECALL for the directory delegation) and proceeds immediately
// (does not block).
//
// This enables the pattern: "client B modifies directory -> server recalls
// delegation from client A -> client A gets CB_RECALL -> client B's operation
// proceeds immediately".
//
// Caller must NOT hold sm.mu (method acquires RLock, then Lock for recall).
func (sm *StateManager) ShouldRecallDirDelegation(dirFH []byte, clientID uint64) {
	sm.mu.RLock()
	delegs := sm.delegByFile[string(dirFH)]
	if len(delegs) == 0 {
		sm.mu.RUnlock()
		return
	}

	// Find directory delegations from different clients
	var toRecall []*DelegationState
	for _, deleg := range delegs {
		if deleg.IsDirectory && !deleg.Revoked && !deleg.RecallSent && deleg.ClientID != clientID {
			toRecall = append(toRecall, deleg)
		}
	}
	sm.mu.RUnlock()

	// Recall each conflicting delegation (non-blocking)
	for _, deleg := range toRecall {
		sm.RecallDirDelegation(deleg, "conflict")
	}
}

// cleanupDirDelegation stops the batch timer and clears pending notifications
// for a directory delegation. Called during client cleanup.
//
// Caller must hold sm.mu (this is called from purge/evict paths).
func (sm *StateManager) cleanupDirDelegation(deleg *DelegationState) {
	if !deleg.IsDirectory {
		return
	}

	deleg.NotifMu.Lock()
	if deleg.BatchTimer != nil {
		deleg.BatchTimer.Stop()
		deleg.BatchTimer = nil
	}
	deleg.PendingNotifs = nil
	deleg.NotifMu.Unlock()
}

// SetMaxDelegations sets the maximum number of outstanding delegations.
// When the limit is reached, new delegation requests are refused.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) SetMaxDelegations(n int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.maxDelegations = n
}

// SetDirDelegBatchWindow sets the notification batching window duration.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) SetDirDelegBatchWindow(d time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.dirDelegBatchWindow = d
}
