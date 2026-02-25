// Package handlers provides SMB2 command handlers and session management.
//
// This file implements SMB2.1+ lease management integrated with the unified lock manager.
// Leases provide client caching permissions using Read/Write/Handle flags.
//
// Reference: MS-SMB2 2.2.13.2.8 SMB2_CREATE_REQUEST_LEASE_V2
package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// SMB2 Lease Constants [MS-SMB2] 2.2.13.2.8
// ============================================================================

const (
	// LeaseV1ContextSize is the size of the SMB2_CREATE_REQUEST_LEASE context
	LeaseV1ContextSize = 32

	// LeaseV2ContextSize is the size of the SMB2_CREATE_REQUEST_LEASE_V2 context
	LeaseV2ContextSize = 52

	// LeaseBreakNotificationSize is the size of a lease break notification [MS-SMB2] 2.2.23.2
	LeaseBreakNotificationSize = 44

	// LeaseBreakAckSize is the size of a lease break acknowledgment [MS-SMB2] 2.2.24.2
	LeaseBreakAckSize = 36
)

// Lease break notification flags
const (
	// LeaseBreakFlagAckRequired indicates the client must acknowledge the break
	LeaseBreakFlagAckRequired uint32 = 0x01
)

// ============================================================================
// Lease Break Notifier Interface
// ============================================================================

// LeaseBreakNotifier is called when a lease break needs to be sent to a client.
// The implementation should send an SMB2 LEASE_BREAK_NOTIFICATION to the session.
type LeaseBreakNotifier interface {
	// SendLeaseBreak sends a lease break notification to the client.
	// sessionID identifies the session, leaseKey is the 128-bit lease identifier.
	// currentState is the client's current state, newState is the target state.
	// epoch is the SMB3 epoch counter.
	SendLeaseBreak(sessionID uint64, leaseKey [16]byte, currentState, newState uint32, epoch uint16) error
}

// ============================================================================
// Lease Request/Response Types [MS-SMB2] 2.2.13.2.8
// ============================================================================

// LeaseCreateContext represents an SMB2_CREATE_REQUEST_LEASE_V2 context.
//
// **Wire Format (52 bytes):**
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       16    LeaseKey         Client-generated 128-bit key
//	16      4     LeaseState       Requested R/W/H state
//	20      4     Flags            Reserved (0)
//	24      8     LeaseDuration    Reserved (0)
//	32      16    ParentLeaseKey   Parent directory lease key (SMB3)
//	48      2     Epoch            State change counter
//	50      2     Reserved         Reserved (0)
type LeaseCreateContext struct {
	LeaseKey       [16]byte
	LeaseState     uint32
	Flags          uint32
	LeaseDuration  uint64
	ParentLeaseKey [16]byte
	Epoch          uint16
}

// DecodeLeaseCreateContext parses an SMB2_CREATE_REQUEST_LEASE_V2 context.
func DecodeLeaseCreateContext(data []byte) (*LeaseCreateContext, error) {
	if len(data) < LeaseV2ContextSize {
		if len(data) >= LeaseV1ContextSize {
			// V1 format (32 bytes) - no parent key or epoch
			return decodeLeaseV1Context(data)
		}
		return nil, fmt.Errorf("lease context too short: %d bytes", len(data))
	}

	ctx := &LeaseCreateContext{
		LeaseState:    binary.LittleEndian.Uint32(data[16:20]),
		Flags:         binary.LittleEndian.Uint32(data[20:24]),
		LeaseDuration: binary.LittleEndian.Uint64(data[24:32]),
		Epoch:         binary.LittleEndian.Uint16(data[48:50]),
	}
	copy(ctx.LeaseKey[:], data[0:16])
	copy(ctx.ParentLeaseKey[:], data[32:48])

	return ctx, nil
}

// decodeLeaseV1Context parses an SMB2_CREATE_REQUEST_LEASE (V1) context.
func decodeLeaseV1Context(data []byte) (*LeaseCreateContext, error) {
	ctx := &LeaseCreateContext{
		LeaseState:    binary.LittleEndian.Uint32(data[16:20]),
		Flags:         binary.LittleEndian.Uint32(data[20:24]),
		LeaseDuration: binary.LittleEndian.Uint64(data[24:32]),
		Epoch:         0, // V1 has no epoch
	}
	copy(ctx.LeaseKey[:], data[0:16])
	// V1 has no parent lease key

	return ctx, nil
}

// EncodeLeaseResponseContext encodes an SMB2_CREATE_RESPONSE_LEASE_V2 context.
func EncodeLeaseResponseContext(leaseKey [16]byte, leaseState uint32, flags uint32, epoch uint16) []byte {
	buf := make([]byte, LeaseV2ContextSize)
	copy(buf[0:16], leaseKey[:])
	binary.LittleEndian.PutUint32(buf[16:20], leaseState)
	binary.LittleEndian.PutUint32(buf[20:24], flags)
	// LeaseDuration (8 bytes) = 0
	// ParentLeaseKey (16 bytes) = 0
	binary.LittleEndian.PutUint16(buf[48:50], epoch)
	// Reserved (2 bytes) = 0
	return buf
}

// ============================================================================
// Lease Break Notification [MS-SMB2] 2.2.23.2
// ============================================================================

// LeaseBreakNotification represents an SMB2 Lease Break Notification.
//
// **Wire Format (44 bytes):**
//
//	Offset  Size  Field              Description
//	------  ----  -----------------  ----------------------------------
//	0       2     StructureSize      Always 44
//	2       2     NewEpoch           New epoch value
//	4       4     Flags              ACK_REQUIRED flag
//	8       16    LeaseKey           Lease identifier
//	24      4     CurrentLeaseState  What client currently has
//	28      4     NewLeaseState      What client should break to
//	32      12    Reserved           Reserved (0)
type LeaseBreakNotification struct {
	NewEpoch          uint16
	Flags             uint32
	LeaseKey          [16]byte
	CurrentLeaseState uint32
	NewLeaseState     uint32
}

// Encode serializes the LeaseBreakNotification to wire format.
func (n *LeaseBreakNotification) Encode() []byte {
	buf := make([]byte, LeaseBreakNotificationSize)
	binary.LittleEndian.PutUint16(buf[0:2], LeaseBreakNotificationSize) // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], n.NewEpoch)
	binary.LittleEndian.PutUint32(buf[4:8], n.Flags)
	copy(buf[8:24], n.LeaseKey[:])
	binary.LittleEndian.PutUint32(buf[24:28], n.CurrentLeaseState)
	binary.LittleEndian.PutUint32(buf[28:32], n.NewLeaseState)
	// Reserved bytes 32-44 are already zero
	return buf
}

// ============================================================================
// Lease Break Acknowledgment [MS-SMB2] 2.2.24.2
// ============================================================================

// LeaseBreakAcknowledgment represents an SMB2 Lease Break Acknowledgment.
//
// **Wire Format (36 bytes):**
//
//	Offset  Size  Field          Description
//	------  ----  -------------  ----------------------------------
//	0       2     StructureSize  Always 36
//	2       2     Reserved       Reserved (0)
//	4       4     Flags          Reserved (0)
//	8       16    LeaseKey       Lease identifier
//	24      4     LeaseState     State client is acknowledging
//	28      8     Reserved       Reserved (0)
type LeaseBreakAcknowledgment struct {
	LeaseKey   [16]byte
	LeaseState uint32
}

// DecodeLeaseBreakAcknowledgment parses an SMB2 Lease Break Acknowledgment.
func DecodeLeaseBreakAcknowledgment(data []byte) (*LeaseBreakAcknowledgment, error) {
	if len(data) < LeaseBreakAckSize {
		return nil, fmt.Errorf("lease break ack too short: %d bytes", len(data))
	}

	structSize := binary.LittleEndian.Uint16(data[0:2])
	if structSize != LeaseBreakAckSize {
		return nil, fmt.Errorf("invalid lease break ack structure size: %d", structSize)
	}

	ack := &LeaseBreakAcknowledgment{
		LeaseState: binary.LittleEndian.Uint32(data[24:28]),
	}
	copy(ack.LeaseKey[:], data[8:24])

	return ack, nil
}

// EncodeLeaseBreakResponse encodes an SMB2 Lease Break Response.
func EncodeLeaseBreakResponse(leaseKey [16]byte, leaseState uint32) []byte {
	buf := make([]byte, LeaseBreakAckSize)
	binary.LittleEndian.PutUint16(buf[0:2], LeaseBreakAckSize) // StructureSize
	// Reserved (2 bytes) = 0
	// Flags (4 bytes) = 0
	copy(buf[8:24], leaseKey[:])
	binary.LittleEndian.PutUint32(buf[24:28], leaseState)
	// Reserved (8 bytes) = 0
	return buf
}

// ============================================================================
// OplockManager Lease Methods
// ============================================================================

// RequestLease acquires a lease through the unified lock manager.
// This is the SMB2.1+ lease API (preferred over oplocks).
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file handle for the lease
//   - leaseKey: Client-generated 128-bit key identifying the lease
//   - sessionID: The SMB session ID (for break notifications)
//   - clientID: The connection tracker client ID
//   - shareName: The share name
//   - requestedState: Requested R/W/H state flags
//   - isDirectory: True if the target is a directory
//
// Returns the granted state, epoch, and any error.
func (m *OplockManager) RequestLease(
	ctx context.Context,
	fileHandle lock.FileHandle,
	leaseKey [16]byte,
	sessionID uint64,
	clientID string,
	shareName string,
	requestedState uint32,
	isDirectory bool,
) (grantedState uint32, epoch uint16, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Validate requested state
	if isDirectory {
		if !lock.IsValidDirectoryLeaseState(requestedState) {
			logger.Debug("Lease: invalid directory state requested",
				"leaseKey", fmt.Sprintf("%x", leaseKey),
				"requestedState", lock.LeaseStateToString(requestedState))
			return lock.LeaseStateNone, 0, nil
		}
	} else {
		if !lock.IsValidFileLeaseState(requestedState) {
			logger.Debug("Lease: invalid file state requested",
				"leaseKey", fmt.Sprintf("%x", leaseKey),
				"requestedState", lock.LeaseStateToString(requestedState))
			return lock.LeaseStateNone, 0, nil
		}
	}

	// Build owner ID for cross-protocol visibility
	ownerID := fmt.Sprintf("smb:lease:%x", leaseKey)

	// ========================================================================
	// Cross-Protocol Check: NLM byte-range locks
	// ========================================================================
	//
	// Per CONTEXT.md: "NFS lock vs SMB Write lease: Deny SMB immediately"
	// NFS byte-range locks are explicit and win over opportunistic SMB leases.
	// We check NLM locks BEFORE checking for existing SMB leases because:
	// 1. NLM locks have priority (explicit vs opportunistic)
	// 2. Denying immediately is simpler than breaking
	// 3. This matches the "NFS locks win" semantic

	if m.lockStore != nil {
		conflicts, err := checkNLMLocksForLeaseConflict(ctx, m.lockStore, fileHandle, requestedState)
		if err != nil {
			// Fail open - don't block lease request due to query failure
			// Log at WARN since this is unexpected
			logger.Warn("Lease: failed to check NLM locks", "error", err)
		} else if len(conflicts) > 0 {
			// Log at INFO per CONTEXT.md (cross-protocol conflicts are working as designed)
			logger.Info("Lease: denied due to NLM lock",
				"leaseKey", fmt.Sprintf("%x", leaseKey),
				"requestedState", lock.LeaseStateToString(requestedState),
				"nlmLock", formatNLMLockInfo(conflicts[0]))

			// Record cross-protocol conflict metric
			lock.RecordCrossProtocolConflict(lock.InitiatorSMB, lock.ConflictingNFSLock, lock.ResolutionDenied)

			// Return None - caller will handle STATUS_LOCK_NOT_GRANTED
			// We return nil error because this is an expected outcome, not a failure
			return lock.LeaseStateNone, 0, nil
		}
	}

	// Check for existing lease with same key
	existing := m.findLeaseByKey(ctx, fileHandle, leaseKey)
	if existing != nil {
		// Same lease key - upgrade/maintain (no break to self)
		return m.upgradeLeaseState(ctx, existing, requestedState)
	}

	// Check for conflicting leases (different key)
	if conflict := m.checkLeaseConflict(ctx, fileHandle, requestedState, leaseKey); conflict != nil {
		// Initiate break to conflicting lease holder
		m.initiateLeaseBreak(conflict, m.calculateBreakToState(requestedState))
		return lock.LeaseStateNone, 0, nil // Caller retries after break
	}

	// Grant new lease
	leaseLock := lock.NewUnifiedLock(
		lock.LockOwner{
			OwnerID:   ownerID,
			ClientID:  clientID,
			ShareName: shareName,
		},
		fileHandle,
		0, 0, // Whole file
		lock.LockTypeShared, // Base type; lease flags determine actual behavior
	)
	leaseLock.Lease = &lock.OpLock{
		LeaseKey:   leaseKey,
		LeaseState: requestedState,
		Epoch:      1,
	}

	// Track session for break notifications
	m.sessionMap[fmt.Sprintf("%x", leaseKey)] = sessionID

	// Persist
	pl := lock.ToPersistedLock(leaseLock, 0)
	if err := m.lockStore.PutLock(ctx, pl); err != nil {
		return lock.LeaseStateNone, 0, err
	}

	m.invalidateCache()

	logger.Debug("Lease: granted",
		"leaseKey", fmt.Sprintf("%x", leaseKey),
		"state", lock.LeaseStateToString(requestedState),
		"fileHandle", fileHandle)

	return requestedState, 1, nil
}

// AcknowledgeLeaseBreak handles the client's response to a lease break.
//
// Parameters:
//   - ctx: Context for cancellation
//   - leaseKey: The lease key from the break acknowledgment
//   - acknowledgedState: The state the client is acknowledging
//
// Returns an error if the acknowledgment is invalid.
func (m *OplockManager) AcknowledgeLeaseBreak(
	ctx context.Context,
	leaseKey [16]byte,
	acknowledgedState uint32,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find lease by key
	lease := m.findLeaseByKeyGlobal(ctx, leaseKey)
	if lease == nil {
		return fmt.Errorf("no lease with key %x", leaseKey)
	}

	if lease.Lease == nil || !lease.Lease.Breaking {
		return fmt.Errorf("no break pending for lease %x", leaseKey)
	}

	// Client must acknowledge to expected level or lower
	if acknowledgedState > lease.Lease.BreakToState {
		return fmt.Errorf("invalid acknowledgment: state 0x%x > expected 0x%x",
			acknowledgedState, lease.Lease.BreakToState)
	}

	// Update lease state
	oldState := lease.Lease.LeaseState
	lease.Lease.LeaseState = acknowledgedState
	lease.Lease.Breaking = false
	lease.Lease.BreakToState = 0
	lease.Lease.Epoch++
	lease.Lease.BreakStarted = time.Time{} // Clear break start time

	// Persist or delete
	if acknowledgedState == lock.LeaseStateNone {
		if err := m.lockStore.DeleteLock(ctx, lease.ID); err != nil {
			return err
		}
		delete(m.sessionMap, fmt.Sprintf("%x", leaseKey))
	} else {
		pl := lock.ToPersistedLock(lease, 0)
		if err := m.lockStore.PutLock(ctx, pl); err != nil {
			return err
		}
	}

	// Clear break tracking
	delete(m.activeBreaks, fmt.Sprintf("%x", leaseKey))
	m.invalidateCache()

	logger.Debug("Lease: break acknowledged",
		"leaseKey", fmt.Sprintf("%x", leaseKey),
		"oldState", lock.LeaseStateToString(oldState),
		"newState", lock.LeaseStateToString(acknowledgedState))

	return nil
}

// ReleaseLease releases a lease when a file is closed.
//
// Parameters:
//   - ctx: Context for cancellation
//   - leaseKey: The lease key to release
func (m *OplockManager) ReleaseLease(ctx context.Context, leaseKey [16]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find lease by key
	lease := m.findLeaseByKeyGlobal(ctx, leaseKey)
	if lease == nil {
		return nil // Already released
	}

	// Delete from store
	if err := m.lockStore.DeleteLock(ctx, lease.ID); err != nil {
		return err
	}

	// Clean up tracking
	keyHex := fmt.Sprintf("%x", leaseKey)
	delete(m.activeBreaks, keyHex)
	delete(m.sessionMap, keyHex)
	m.invalidateCache()

	logger.Debug("Lease: released",
		"leaseKey", fmt.Sprintf("%x", leaseKey),
		"state", lock.LeaseStateToString(lease.Lease.LeaseState))

	return nil
}

// GetLeaseState returns the current lease state for a given key.
func (m *OplockManager) GetLeaseState(ctx context.Context, leaseKey [16]byte) (uint32, uint16, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	lease := m.findLeaseByKeyGlobal(ctx, leaseKey)
	if lease == nil || lease.Lease == nil {
		return lock.LeaseStateNone, 0, false
	}

	return lease.Lease.LeaseState, lease.Lease.Epoch, true
}

// ============================================================================
// Helper Methods
// ============================================================================

// findLeaseByKey finds a lease by key on a specific file.
// Must be called with m.mu held.
func (m *OplockManager) findLeaseByKey(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte) *lock.UnifiedLock {
	return m.findLeaseByQuery(ctx, lock.LockQuery{FileID: string(fileHandle)}, leaseKey)
}

// findLeaseByKeyGlobal finds a lease by key across all files.
// Must be called with m.mu held (for read).
func (m *OplockManager) findLeaseByKeyGlobal(ctx context.Context, leaseKey [16]byte) *lock.UnifiedLock {
	return m.findLeaseByQuery(ctx, lock.LockQuery{}, leaseKey)
}

// findLeaseByQuery searches for a lease matching the given key within the query scope.
// Must be called with m.mu held.
func (m *OplockManager) findLeaseByQuery(ctx context.Context, query lock.LockQuery, leaseKey [16]byte) *lock.UnifiedLock {
	if m.lockStore == nil {
		return nil
	}

	isLease := true
	query.IsLease = &isLease

	leases, err := m.lockStore.ListLocks(ctx, query)
	if err != nil {
		logger.Warn("Lease: failed to query leases", "error", err)
		return nil
	}

	for _, pl := range leases {
		if len(pl.LeaseKey) == 16 {
			var key [16]byte
			copy(key[:], pl.LeaseKey)
			if key == leaseKey {
				return lock.FromPersistedLock(pl)
			}
		}
	}

	return nil
}

// checkLeaseConflict checks for conflicting leases on a file.
// Must be called with m.mu held.
func (m *OplockManager) checkLeaseConflict(ctx context.Context, fileHandle lock.FileHandle, requestedState uint32, excludeKey [16]byte) *lock.UnifiedLock {
	if m.lockStore == nil {
		return nil
	}

	// Query leases for this file
	isLease := true
	leases, err := m.lockStore.ListLocks(ctx, lock.LockQuery{
		FileID:  string(fileHandle),
		IsLease: &isLease,
	})
	if err != nil {
		logger.Warn("Lease: failed to query leases for conflict check", "error", err)
		return nil
	}

	// Build a temporary lease info for conflict checking
	requestedInfo := &lock.OpLock{
		LeaseKey:   excludeKey,
		LeaseState: requestedState,
	}

	for _, pl := range leases {
		if len(pl.LeaseKey) != 16 {
			continue
		}

		var key [16]byte
		copy(key[:], pl.LeaseKey)
		if key == excludeKey {
			continue // Skip same key
		}

		el := lock.FromPersistedLock(pl)
		if el.Lease != nil && lock.OpLocksConflict(el.Lease, requestedInfo) {
			return el
		}
	}

	return nil
}

// calculateBreakToState determines the target state for a lease break.
// Must be called with m.mu held.
func (m *OplockManager) calculateBreakToState(requestedState uint32) uint32 {
	// If requester wants Write, break existing to Read or None
	if requestedState&lock.LeaseStateWrite != 0 {
		// Requestor wants write - break existing to Read only
		return lock.LeaseStateRead
	}

	// If requester wants Read, break Write (keep Read)
	if requestedState&lock.LeaseStateRead != 0 {
		return lock.LeaseStateRead
	}

	// Default: break to None
	return lock.LeaseStateNone
}

// upgradeLeaseState handles upgrading/maintaining lease state for same key.
// Must be called with m.mu held.
func (m *OplockManager) upgradeLeaseState(ctx context.Context, existing *lock.UnifiedLock, requestedState uint32) (uint32, uint16, error) {
	if existing.Lease == nil {
		return lock.LeaseStateNone, 0, fmt.Errorf("existing lock is not a lease")
	}

	// If break is in progress, return current breaking state
	if existing.Lease.Breaking {
		return existing.Lease.LeaseState, existing.Lease.Epoch, nil
	}

	// Same or lower state - return current
	if requestedState == existing.Lease.LeaseState {
		return existing.Lease.LeaseState, existing.Lease.Epoch, nil
	}

	// Upgrade attempt - check if we can grant more
	// (Only if no conflicting operations from other clients)
	newState := existing.Lease.LeaseState | requestedState

	// For simplicity, grant the union of states (real implementation
	// would check for conflicts with other operations)
	existing.Lease.LeaseState = newState
	existing.Lease.Epoch++

	// Persist
	pl := lock.ToPersistedLock(existing, 0)
	if err := m.lockStore.PutLock(ctx, pl); err != nil {
		return lock.LeaseStateNone, 0, err
	}

	m.invalidateCache()

	logger.Debug("Lease: upgraded",
		"leaseKey", fmt.Sprintf("%x", existing.Lease.LeaseKey),
		"newState", lock.LeaseStateToString(newState))

	return newState, existing.Lease.Epoch, nil
}

// initiateLeaseBreak starts a lease break and notifies the holder.
// Must be called with m.mu held.
func (m *OplockManager) initiateLeaseBreak(lease *lock.UnifiedLock, breakToState uint32) {
	if lease.Lease == nil {
		return
	}

	if lease.Lease.Breaking {
		return // Already breaking
	}

	lease.Lease.Breaking = true
	lease.Lease.BreakToState = breakToState
	lease.Lease.Epoch++
	lease.Lease.BreakStarted = time.Now()

	logger.Debug("Lease: initiating break",
		"leaseKey", fmt.Sprintf("%x", lease.Lease.LeaseKey),
		"from", lock.LeaseStateToString(lease.Lease.LeaseState),
		"to", lock.LeaseStateToString(breakToState))

	// Persist the breaking state
	pl := lock.ToPersistedLock(lease, 0)
	if err := m.lockStore.PutLock(context.Background(), pl); err != nil {
		logger.Warn("Lease: failed to persist break state", "error", err)
	}

	// Track in active breaks map
	keyHex := fmt.Sprintf("%x", lease.Lease.LeaseKey)
	m.activeBreaks[keyHex] = time.Now()

	// Send break notification async
	if m.leaseNotify != nil {
		// Capture values for goroutine
		sessionID := m.sessionMap[keyHex]
		leaseKey := lease.Lease.LeaseKey
		currentState := lease.Lease.LeaseState
		newState := breakToState
		epoch := lease.Lease.Epoch

		go func() {
			if err := m.leaseNotify.SendLeaseBreak(sessionID, leaseKey, currentState, newState, epoch); err != nil {
				logger.Warn("Lease: failed to send break notification",
					"leaseKey", fmt.Sprintf("%x", leaseKey),
					"error", err)
			}
		}()
	}

	m.invalidateCache()
}

// invalidateCache clears the lease cache.
// Must be called with m.mu held.
func (m *OplockManager) invalidateCache() {
	m.leaseCache = make(map[string][]*lock.UnifiedLock)
	m.cacheValid = false
}

// ============================================================================
// SMB Lease Reclaim for Grace Period Recovery
// ============================================================================

// LeaseReclaimer defines the interface for reclaiming SMB leases during grace period.
// This is typically implemented by MetadataService.
type LeaseReclaimer interface {
	// ReclaimLeaseSMB attempts to reclaim a lease during grace period.
	// Returns the reclaimed lease result or error if lease doesn't exist.
	ReclaimLeaseSMB(
		ctx context.Context,
		handle lock.FileHandle,
		leaseKey [16]byte,
		clientID string,
		requestedState uint32,
	) (*lock.LockResult, error)
}

// HandleLeaseReclaim processes a lease reclaim request during grace period.
//
// This is called when an SMB client reconnects after server restart and wants to
// reclaim previously held leases. SMB2 doesn't have an explicit "reclaim" flag,
// so detection is based on:
//   - Server is in grace period (recently restarted)
//   - Client provides a lease key it previously held
//
// Parameters:
//   - ctx: Context for cancellation
//   - reclaimer: Service that handles lease reclaim (MetadataService)
//   - fileHandle: The file handle for the lease
//   - leaseKey: Client's 128-bit lease key
//   - sessionID: The SMB session ID
//   - clientID: The connection tracker client ID
//   - requestedState: The R/W/H state being reclaimed
//
// Returns the granted state, epoch, and any error.
func (m *OplockManager) HandleLeaseReclaim(
	ctx context.Context,
	reclaimer LeaseReclaimer,
	fileHandle lock.FileHandle,
	leaseKey [16]byte,
	sessionID uint64,
	clientID string,
	requestedState uint32,
) (grantedState uint32, epoch uint16, err error) {
	if reclaimer == nil {
		// No reclaimer available - fall through to normal acquisition
		logger.Debug("Lease: reclaim attempted but no reclaimer available",
			"leaseKey", fmt.Sprintf("%x", leaseKey))
		return lock.LeaseStateNone, 0, nil
	}

	// Try to reclaim via MetadataService
	result, err := reclaimer.ReclaimLeaseSMB(ctx, fileHandle, leaseKey, clientID, requestedState)
	if err != nil {
		// Reclaim failed - this could be ErrLockNotFound (no prior lease)
		// or ErrGracePeriod (not in grace period)
		// Log at DEBUG since this is expected for new leases
		logger.Debug("Lease: reclaim failed, will treat as new request",
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"error", err)
		return lock.LeaseStateNone, 0, nil // Caller should try normal acquisition
	}

	// Reclaim succeeded
	if result != nil && result.Success && result.Lock != nil && result.Lock.Lease != nil {
		// Track session for future break notifications
		m.mu.Lock()
		m.sessionMap[fmt.Sprintf("%x", leaseKey)] = sessionID
		m.invalidateCache()
		m.mu.Unlock()

		logger.Info("Lease: reclaimed during grace period",
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"state", lock.LeaseStateToString(result.Lock.Lease.LeaseState),
			"fileHandle", fileHandle)

		return result.Lock.Lease.LeaseState, result.Lock.Lease.Epoch, nil
	}

	return lock.LeaseStateNone, 0, nil
}

// RequestLeaseWithReclaim tries reclaim first (during grace period), then normal acquisition.
//
// This is the preferred method for lease acquisition that handles both:
//   - Grace period reclaim (for reconnecting clients after server restart)
//   - Normal lease acquisition (for new lease requests)
//
// Parameters:
//   - ctx: Context for cancellation
//   - reclaimer: Service that handles lease reclaim (nil to skip reclaim)
//   - fileHandle: The file handle for the lease
//   - leaseKey: Client-generated 128-bit key
//   - sessionID: The SMB session ID
//   - clientID: The connection tracker client ID
//   - shareName: The share name
//   - requestedState: Requested R/W/H state flags
//   - isDirectory: True if target is a directory
//
// Returns the granted state, epoch, and any error.
func (m *OplockManager) RequestLeaseWithReclaim(
	ctx context.Context,
	reclaimer LeaseReclaimer,
	fileHandle lock.FileHandle,
	leaseKey [16]byte,
	sessionID uint64,
	clientID string,
	shareName string,
	requestedState uint32,
	isDirectory bool,
) (grantedState uint32, epoch uint16, err error) {
	// Try reclaim first if reclaimer is available
	// This handles the case where server just restarted and client is reconnecting
	if reclaimer != nil {
		state, ep, err := m.HandleLeaseReclaim(
			ctx, reclaimer, fileHandle, leaseKey, sessionID, clientID, requestedState)
		if err != nil {
			return lock.LeaseStateNone, 0, err
		}
		if state != lock.LeaseStateNone {
			// Reclaim succeeded
			return state, ep, nil
		}
		// Reclaim didn't find prior lease, continue with normal acquisition
	}

	// Normal lease acquisition
	return m.RequestLease(ctx, fileHandle, leaseKey, sessionID, clientID, shareName, requestedState, isDirectory)
}
