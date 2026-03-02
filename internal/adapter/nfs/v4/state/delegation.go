package state

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// RecentlyRecalledTTL is the duration for which a file is considered
// "recently recalled" after a delegation recall. During this period,
// new delegations will not be granted for the file to prevent
// grant-recall-grant-recall storms (Pitfall 7 from research).
const RecentlyRecalledTTL = 30 * time.Second

// DelegationState represents a granted delegation for a file or directory.
//
// Per RFC 7530 Section 10.4, a delegation allows the server to delegate
// file management to a client for improved caching performance.
// The client can locally service OPEN, CLOSE, LOCK, READ, WRITE
// without server interaction until the delegation is recalled.
//
// Directory delegations (RFC 8881 Section 18.39) additionally carry a
// notification bitmask and a batch of pending notifications that are
// periodically flushed via CB_NOTIFY.
type DelegationState struct {
	// Stateid is the delegation stateid (type tag = 0x03).
	Stateid types.Stateid4

	// ClientID is the server-assigned client identifier that holds this delegation.
	ClientID uint64

	// FileHandle is the file handle of the delegated file or directory.
	FileHandle []byte

	// DelegType is the delegation type: OPEN_DELEGATE_READ or OPEN_DELEGATE_WRITE.
	DelegType uint32

	// RecallSent indicates whether CB_RECALL has been sent for this delegation.
	RecallSent bool

	// RecallTime is when CB_RECALL was sent (zero value if not recalled).
	RecallTime time.Time

	// Revoked indicates whether this delegation has been revoked by the server.
	Revoked bool

	// RecallTimer fires revocation after lease duration since CB_RECALL was sent.
	// Per RFC 7530 Section 10.4.6: "The server MUST NOT revoke the delegation
	// before the lease period has expired."
	RecallTimer *time.Timer

	// ========================================================================
	// Directory delegation fields (zero values for file delegations)
	// Per RFC 8881 Section 18.39 (GET_DIR_DELEGATION)
	// ========================================================================

	// IsDirectory is true for directory delegations, false for file delegations.
	IsDirectory bool

	// NotificationMask is the NOTIFY4_* bitmask from GET_DIR_DELEGATION.
	// Each bit indicates a notification type the client wants to receive.
	NotificationMask uint32

	// CookieVerf is the cookie verifier for directory delegations.
	CookieVerf [8]byte

	// PendingNotifs holds batched notifications awaiting flush via CB_NOTIFY.
	// Protected by NotifMu (separate from sm.mu to avoid holding the global
	// lock during backchannel sends).
	PendingNotifs []DirNotification

	// NotifMu protects PendingNotifs and BatchTimer.
	// Lock ordering: sm.mu before NotifMu (never reverse).
	NotifMu sync.Mutex

	// BatchTimer is the notification batch flush timer.
	// When it fires, accumulated notifications are flushed via CB_NOTIFY.
	BatchTimer *time.Timer

	// RecallReason records why this delegation was recalled for metrics/logging.
	// Values: "conflict", "resource_pressure", "admin", "directory_deleted".
	RecallReason string

	// LockManagerDelegID is the UUID of the corresponding delegation in the
	// shared LockManager. Empty if no LockManager delegation was created.
	// Used for cleanup when the NFS delegation is returned or revoked.
	LockManagerDelegID string
}

// DirNotification represents a single directory change notification to be
// batched and sent via CB_NOTIFY per RFC 8881 Section 20.4.
type DirNotification struct {
	// Type is the notification type (NOTIFY4_ADD_ENTRY, NOTIFY4_REMOVE_ENTRY, etc.).
	Type uint32

	// EntryName is the name of the affected directory entry.
	EntryName string

	// Cookie is the readdir cookie for the entry.
	Cookie uint64

	// Attrs is pre-encoded fattr4 bytes (optional, for attribute change notifications).
	Attrs []byte

	// NewName is the new name for RENAME notifications (EntryName is the old name).
	NewName string

	// NewDirFH is the destination directory handle for cross-directory RENAME.
	NewDirFH []byte

	// OriginClientID is the client ID that caused this notification.
	// Used for conflict-based recall: if a different client modifies the
	// directory, the delegation is recalled from other holders.
	// Zero means unknown (no conflict recall triggered).
	OriginClientID uint64
}

// StartRecallTimer starts a timer that fires onExpiry after leaseDuration.
// If a timer already exists, it is stopped first (idempotent).
// The onExpiry callback should call StateManager.RevokeDelegation.
func (d *DelegationState) StartRecallTimer(leaseDuration time.Duration, onExpiry func()) {
	if d.RecallTimer != nil {
		d.RecallTimer.Stop()
	}
	d.RecallTimer = time.AfterFunc(leaseDuration, onExpiry)
}

// StopRecallTimer stops the recall timer if it exists.
// Called when a delegation is returned voluntarily (DELEGRETURN)
// to prevent revocation of a delegation the client returned in time.
func (d *DelegationState) StopRecallTimer() {
	if d.RecallTimer != nil {
		d.RecallTimer.Stop()
		d.RecallTimer = nil
	}
}

// ============================================================================
// Delegation Stateid Mapping
// ============================================================================

// GetStateidForDelegation returns the NFS Stateid4 for a LockManager delegation ID.
// Used by NFSBreakHandler to look up the NFS stateid when dispatching CB_RECALL.
func (sm *StateManager) GetStateidForDelegation(delegationID string) (types.Stateid4, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stateid, found := sm.delegStateidMap[delegationID]
	return stateid, found
}

// ============================================================================
// Delegation Operations on StateManager
// ============================================================================

// countActiveDelegations returns the number of non-revoked delegations.
// Revoked delegations are kept in delegByOther for stale stateid detection
// but should not count toward the maxDelegations limit.
//
// Caller must hold sm.mu.
func (sm *StateManager) countActiveDelegations() int {
	count := 0
	for _, deleg := range sm.delegByOther {
		if !deleg.Revoked {
			count++
		}
	}
	return count
}

// removeDelegFromFile removes a delegation from the delegByFile map.
// Cleans up the map entry if no delegations remain for the file.
//
// Caller must hold sm.mu.
func (sm *StateManager) removeDelegFromFile(deleg *DelegationState) {
	fhKey := string(deleg.FileHandle)
	delegs := sm.delegByFile[fhKey]
	for i, d := range delegs {
		if d == deleg {
			sm.delegByFile[fhKey] = append(delegs[:i], delegs[i+1:]...)
			break
		}
	}
	if len(sm.delegByFile[fhKey]) == 0 {
		delete(sm.delegByFile, fhKey)
	}
}

// GrantDelegation creates a new delegation for a client on a file.
//
// It generates a delegation stateid with type tag 0x03, creates a
// DelegationState, and stores it in both the delegByOther and
// delegByFile maps.
//
// Returns the DelegationState for the caller to encode in the OPEN response.
//
// Caller must NOT hold sm.mu (method acquires it).
func (sm *StateManager) GrantDelegation(clientID uint64, fileHandle []byte, delegType uint32) *DelegationState {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Check total active delegation count against limit
	if sm.maxDelegations > 0 && sm.countActiveDelegations() >= sm.maxDelegations {
		return nil
	}

	other := sm.generateStateidOther(StateTypeDeleg)
	stateid := types.Stateid4{
		Seqid: 1,
		Other: other,
	}

	fhCopy := make([]byte, len(fileHandle))
	copy(fhCopy, fileHandle)

	deleg := &DelegationState{
		Stateid:    stateid,
		ClientID:   clientID,
		FileHandle: fhCopy,
		DelegType:  delegType,
	}

	sm.delegByOther[other] = deleg

	fhKey := string(fileHandle)
	sm.delegByFile[fhKey] = append(sm.delegByFile[fhKey], deleg)

	if sm.lockManager != nil {
		lmDelegType := lock.DelegTypeRead
		if deleg.DelegType == types.OPEN_DELEGATE_WRITE {
			lmDelegType = lock.DelegTypeWrite
		}
		lockDeleg := lock.NewDelegation(lmDelegType, fmt.Sprintf("%d", clientID), "", false)
		if err := sm.lockManager.GrantDelegation(fhKey, lockDeleg); err != nil {
			logger.Debug("LockManager delegation grant failed, continuing with local state",
				"error", err)
		} else {
			sm.delegStateidMap[lockDeleg.DelegationID] = stateid
			deleg.LockManagerDelegID = lockDeleg.DelegationID
		}
	}

	logger.Info("Delegation granted",
		"client_id", clientID,
		"deleg_type", delegType,
		"stateid_seqid", stateid.Seqid)

	return deleg
}

// ReturnDelegation removes a delegation by its stateid.
//
// Per RFC 7530 Section 16.8, DELEGRETURN returns a delegation to the server.
// This method removes the delegation from both delegByOther and delegByFile maps.
//
// For directory delegations, pending notifications are flushed via CB_NOTIFY
// before the delegation is removed (ensuring the client receives all pending
// changes before the delegation is acknowledged as returned).
//
// Idempotent: returning an already-returned delegation succeeds with nil error
// (per Pitfall 3 from research -- race between DELEGRETURN and CB_RECALL).
//
// Returns nil on success. Returns NFS4ERR_STALE_STATEID if the stateid
// is from a previous server incarnation.
//
// Caller must NOT hold sm.mu (method acquires it).
func (sm *StateManager) ReturnDelegation(stateid *types.Stateid4) error {
	sm.mu.Lock()
	deleg, exists := sm.delegByOther[stateid.Other]
	if !exists {
		isCurrentEpoch := sm.isCurrentEpoch(stateid.Other)
		sm.mu.Unlock()
		if !isCurrentEpoch {
			return ErrStaleStateid
		}
		// Current epoch but not found: already returned (idempotent)
		return nil
	}

	deleg.StopRecallTimer()

	// For directory delegations: flush pending notifications before removal.
	// Must release sm.mu because flushDirNotifications needs RLock for backchannel.
	if deleg.IsDirectory {
		deleg.RecallSent = true

		deleg.NotifMu.Lock()
		if deleg.BatchTimer != nil {
			deleg.BatchTimer.Stop()
			deleg.BatchTimer = nil
		}
		deleg.NotifMu.Unlock()

		sm.mu.Unlock()
		sm.flushDirNotifications(deleg)
		sm.mu.Lock()
	}

	delete(sm.delegByOther, stateid.Other)
	sm.removeDelegFromFile(deleg)

	lmDelegID := deleg.LockManagerDelegID
	fhKey := string(deleg.FileHandle)
	if lmDelegID != "" {
		delete(sm.delegStateidMap, lmDelegID)
	}

	delegKind := "file"
	if deleg.IsDirectory {
		delegKind = "directory"
	}

	lockMgr := sm.lockManager

	if deleg.Revoked {
		logger.Info("Revoked delegation returned by client",
			"client_id", deleg.ClientID,
			"deleg_type", deleg.DelegType)
	} else {
		logger.Info("Delegation returned",
			"client_id", deleg.ClientID,
			"deleg_type", deleg.DelegType,
			"kind", delegKind)
	}

	sm.mu.Unlock()

	// Return delegation to LockManager outside sm.mu to avoid deadlock.
	if lockMgr != nil && lmDelegID != "" {
		_ = lockMgr.ReturnDelegation(fhKey, lmDelegID)
	}

	return nil
}

// GetDelegationsForFile returns all active delegations for a given file handle.
//
// Used by conflict detection (Plan 11-03) to check if another client holds
// a delegation before granting a new OPEN.
//
// Caller must NOT hold sm.mu (method acquires it with RLock).
func (sm *StateManager) GetDelegationsForFile(fileHandle []byte) []*DelegationState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	delegs := sm.delegByFile[string(fileHandle)]
	if len(delegs) == 0 {
		return nil
	}

	// Return a copy of the slice to avoid caller mutations
	result := make([]*DelegationState, len(delegs))
	copy(result, delegs)
	return result
}

// countOpensOnFile counts the number of open states on a file that belong to
// clients OTHER than the specified clientID.
//
// Used for delegation grant decisions: if other clients have opens on the file,
// a delegation should not be granted (conflict is imminent).
//
// Caller must hold sm.mu.
func (sm *StateManager) countOpensOnFile(fileHandle []byte, excludeClientID uint64) int {
	count := 0
	for _, openState := range sm.openStateByOther {
		if bytes.Equal(openState.FileHandle, fileHandle) &&
			openState.Owner != nil &&
			openState.Owner.ClientID != excludeClientID {
			count++
		}
	}
	return count
}

// ============================================================================
// Delegation Grant Decision
// ============================================================================

// ShouldGrantDelegation determines whether a delegation should be granted
// for a client opening a file.
//
// Policy (simple, per research recommendation -- no heuristics):
//  1. Client must have a non-empty callback address
//  2. No other clients may have opens on the file
//  3. No existing delegations from other clients on the file
//  4. Same client must not already have a delegation (avoid double-grant)
//  5. Grant type based on shareAccess: WRITE access -> WRITE delegation, else READ
//
// Returns the delegation type and whether to grant.
//
// Caller must NOT hold sm.mu (method acquires RLock).
func (sm *StateManager) ShouldGrantDelegation(clientID uint64, fileHandle []byte, shareAccess uint32) (uint32, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if !sm.delegationsEnabled {
		return types.OPEN_DELEGATE_NONE, false
	}

	client, exists := sm.clientsByID[clientID]
	if !exists {
		return types.OPEN_DELEGATE_NONE, false
	}
	if !client.CBPathUp {
		return types.OPEN_DELEGATE_NONE, false
	}

	if sm.isRecentlyRecalled(fileHandle) {
		return types.OPEN_DELEGATE_NONE, false
	}

	if sm.countOpensOnFile(fileHandle, clientID) > 0 {
		return types.OPEN_DELEGATE_NONE, false
	}

	for _, deleg := range sm.delegByFile[string(fileHandle)] {
		if !deleg.Revoked {
			return types.OPEN_DELEGATE_NONE, false
		}
	}

	if shareAccess&types.OPEN4_SHARE_ACCESS_WRITE != 0 {
		return types.OPEN_DELEGATE_WRITE, true
	}
	return types.OPEN_DELEGATE_READ, true
}

// ============================================================================
// Delegation Conflict Detection
// ============================================================================

// CheckDelegationConflict checks whether an OPEN by a client conflicts with
// existing delegations held by other clients.
//
// Conflict rules:
//   - WRITE delegation: any access by another client is a conflict
//   - READ delegation + WRITE access: conflict
//   - READ delegation + READ-only access: no conflict (multiple readers allowed)
//
// On conflict, marks the delegation as recalled and launches an async
// goroutine to send CB_RECALL (does NOT hold the lock during TCP).
//
// Returns true if a conflict was detected (caller should return NFS4ERR_DELAY).
//
// Caller must NOT hold sm.mu (method acquires write Lock).
func (sm *StateManager) CheckDelegationConflict(fileHandle []byte, clientID uint64, shareAccess uint32) (bool, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, deleg := range sm.delegByFile[string(fileHandle)] {
		if deleg.ClientID == clientID || deleg.Revoked {
			continue
		}

		isConflict := deleg.DelegType == types.OPEN_DELEGATE_WRITE ||
			(deleg.DelegType == types.OPEN_DELEGATE_READ && shareAccess&types.OPEN4_SHARE_ACCESS_WRITE != 0)

		if isConflict {
			deleg.RecallSent = true
			deleg.RecallTime = time.Now()

			go sm.sendRecall(deleg)

			return true, nil
		}
	}

	return false, nil
}

// startRevocationTimer starts a recall timer on the delegation that will revoke it
// on expiry. Uses a short timeout (5s) for failure cases or the full lease duration
// for successful recalls.
func (sm *StateManager) startRevocationTimer(deleg *DelegationState, timeout time.Duration) {
	deleg.StartRecallTimer(timeout, func() {
		sm.RevokeDelegation(deleg.Stateid.Other)
	})
}

// sendRecall sends a CB_RECALL to the delegation holder.
//
// For v4.1 clients, the recall is enqueued to the BackchannelSender goroutine
// which sends it over a back-bound TCP connection.
// For v4.0 clients, the recall uses the dial-out path (SendCBRecall).
//
// IMPORTANT: This must NOT hold sm.mu during the TCP call.
func (sm *StateManager) sendRecall(deleg *DelegationState) {
	sender := sm.getBackchannelSender(deleg.ClientID)
	if sender != nil {
		sm.sendRecallV41(deleg, sender)
		return
	}
	sm.sendRecallV40(deleg)
}

// sendRecallV41 sends CB_RECALL via the v4.1 BackchannelSender.
func (sm *StateManager) sendRecallV41(deleg *DelegationState, sender *BackchannelSender) {
	recallOp := EncodeCBRecallOp(&deleg.Stateid, false, deleg.FileHandle)

	resultCh := make(chan error, 1)
	req := CallbackRequest{
		OpCode:   types.OP_CB_RECALL,
		Payload:  recallOp,
		ResultCh: resultCh,
	}

	if !sender.Enqueue(req) {
		logger.Warn("CB_RECALL: backchannel queue full, starting short revocation timer",
			"client_id", deleg.ClientID)
		sm.startRevocationTimer(deleg, 5*time.Second)
		return
	}

	select {
	case err := <-resultCh:
		if err != nil {
			logger.Warn("CB_RECALL (v4.1) failed",
				"client_id", deleg.ClientID,
				"error", err)
			sm.startRevocationTimer(deleg, 5*time.Second)
			return
		}
		sm.startRevocationTimer(deleg, sm.leaseDuration)
		logger.Debug("CB_RECALL (v4.1) sent successfully",
			"client_id", deleg.ClientID,
			"deleg_type", deleg.DelegType)

	case <-time.After(30 * time.Second):
		logger.Warn("CB_RECALL (v4.1) result timeout",
			"client_id", deleg.ClientID)
		sm.startRevocationTimer(deleg, 5*time.Second)
	}
}

// sendRecallV40 sends CB_RECALL via the v4.0 dial-out path.
func (sm *StateManager) sendRecallV40(deleg *DelegationState) {
	sm.mu.RLock()
	client, exists := sm.clientsByID[deleg.ClientID]
	var callback CallbackInfo
	if exists {
		callback = client.Callback
	}
	sm.mu.RUnlock()

	if !exists || callback.Addr == "" {
		logger.Warn("CB_RECALL: no callback info for client",
			"client_id", deleg.ClientID)
		sm.startRevocationTimer(deleg, 5*time.Second)
		return
	}

	err := SendCBRecall(context.Background(), callback, &deleg.Stateid, false, deleg.FileHandle)
	if err != nil {
		logger.Warn("CB_RECALL failed",
			"client_id", deleg.ClientID,
			"error", err)
		sm.startRevocationTimer(deleg, 5*time.Second)
		sm.mu.Lock()
		if c, ok := sm.clientsByID[deleg.ClientID]; ok {
			c.CBPathUp = false
		}
		sm.mu.Unlock()
		return
	}

	sm.startRevocationTimer(deleg, sm.leaseDuration)
	logger.Debug("CB_RECALL sent successfully",
		"client_id", deleg.ClientID,
		"deleg_type", deleg.DelegType)
}

// ============================================================================
// EncodeDelegation
// ============================================================================

// EncodeDelegation encodes an open_delegation4 into the given buffer.
//
// If deleg is nil, writes OPEN_DELEGATE_NONE (uint32 = 0).
// Otherwise, encodes the full delegation response including stateid,
// recall flag, ACE permissions, and (for write delegations) space limit.
//
// Wire format per RFC 7530 Section 16.16:
//
//	open_delegation4 union:
//	  OPEN_DELEGATE_NONE:  just the discriminant (0)
//	  OPEN_DELEGATE_READ:  stateid4 + recall(bool) + nfsace4
//	  OPEN_DELEGATE_WRITE: stateid4 + recall(bool) + nfs_space_limit4 + nfsace4
func EncodeDelegation(buf *bytes.Buffer, deleg *DelegationState) {
	if deleg == nil {
		_ = xdr.WriteUint32(buf, types.OPEN_DELEGATE_NONE)
		return
	}

	// Write delegation type discriminant
	_ = xdr.WriteUint32(buf, deleg.DelegType)

	// Encode stateid4
	types.EncodeStateid4(buf, &deleg.Stateid)

	// recall: bool (false at grant time -- not being recalled)
	_ = xdr.WriteBool(buf, false)

	// For WRITE delegations: encode nfs_space_limit4
	if deleg.DelegType == types.OPEN_DELEGATE_WRITE {
		// limitby: NFS_LIMIT_SIZE (1)
		_ = xdr.WriteUint32(buf, types.NFS_LIMIT_SIZE)
		// filesize: unlimited (0xFFFFFFFFFFFFFFFF)
		_ = xdr.WriteUint64(buf, 0xFFFFFFFFFFFFFFFF)
	}

	// Encode nfsace4
	// type: ACE4_ACCESS_ALLOWED_ACE_TYPE (0)
	_ = xdr.WriteUint32(buf, types.ACE4_ACCESS_ALLOWED_ACE_TYPE)
	// flag: 0
	_ = xdr.WriteUint32(buf, 0)

	// access_mask: depends on delegation type
	if deleg.DelegType == types.OPEN_DELEGATE_READ {
		_ = xdr.WriteUint32(buf, types.ACE4_GENERIC_READ)
	} else {
		// WRITE delegation: read + write access
		_ = xdr.WriteUint32(buf, types.ACE4_GENERIC_READ|types.ACE4_GENERIC_WRITE)
	}

	// who: "EVERYONE@"
	_ = xdr.WriteXDRString(buf, "EVERYONE@")
}

// ============================================================================
// ValidateDelegationStateid
// ============================================================================

// ValidateDelegationStateid validates a delegation stateid for CLAIM_DELEGATE_CUR.
//
// It looks up the delegation by the stateid's Other field, validates the boot
// epoch, and returns the DelegationState or an appropriate error.
//
// Caller must NOT hold sm.mu (method acquires RLock).
func (sm *StateManager) ValidateDelegationStateid(stateid *types.Stateid4) (*DelegationState, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	deleg, exists := sm.delegByOther[stateid.Other]
	if !exists {
		if !sm.isCurrentEpoch(stateid.Other) {
			return nil, ErrStaleStateid
		}
		return nil, ErrBadStateid
	}

	// Revoked delegation returns NFS4ERR_BAD_STATEID per RFC 7530 Section 10.4.6
	if deleg.Revoked {
		return nil, ErrBadStateid
	}

	return deleg, nil
}

// ============================================================================
// Recently-Recalled Cache
// ============================================================================

// addRecentlyRecalled adds a file handle to the recently-recalled cache.
// This prevents grant-recall-grant-recall storms (Pitfall 7 from research).
//
// Caller must hold sm.mu.
func (sm *StateManager) addRecentlyRecalled(fileHandle []byte) {
	sm.recentlyRecalled[string(fileHandle)] = time.Now()
}

// isRecentlyRecalled returns true if the file handle was recently recalled
// within the TTL window. Also lazily cleans up expired entries.
//
// Caller must hold sm.mu (RLock or Lock).
func (sm *StateManager) isRecentlyRecalled(fileHandle []byte) bool {
	fhKey := string(fileHandle)
	recallTime, exists := sm.recentlyRecalled[fhKey]
	if !exists {
		return false
	}
	if time.Since(recallTime) > sm.recentlyRecalledTTL {
		delete(sm.recentlyRecalled, fhKey)
		return false
	}
	return true
}
