// Package handlers provides SMB2 command handlers and session management.
//
// This file implements SMB2 opportunistic lock (oplock) management [MS-SMB2] 2.2.23, 2.2.24.
// Oplocks allow clients to cache file data aggressively for better performance.
//
// The OplockManager is integrated with the unified lock manager from pkg/metadata/lock
// to enable cross-protocol visibility (SMB leases visible to NLM).
package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// SMB2 Oplock Constants [MS-SMB2] 2.2.14
// ============================================================================

const (
	// OplockLevelNone means no oplock granted.
	OplockLevelNone uint8 = 0x00

	// OplockLevelII is a shared read-caching oplock.
	// Multiple clients can hold Level II oplocks on the same file.
	// The client can cache read data but must not cache writes.
	OplockLevelII uint8 = 0x01

	// OplockLevelExclusive is an exclusive read/write caching oplock.
	// Only one client can hold an exclusive oplock.
	// The client can cache both reads and writes.
	OplockLevelExclusive uint8 = 0x08

	// OplockLevelBatch is like Exclusive but also allows handle caching.
	// The client can delay close operations for better performance.
	OplockLevelBatch uint8 = 0x09

	// OplockLevelLease indicates the request uses SMB2.1+ lease semantics.
	// Not currently supported - downgraded to traditional oplocks.
	OplockLevelLease uint8 = 0xFF
)

// ============================================================================
// Oplock State Management
// ============================================================================

// OplockState tracks the oplock state for a single file.
type OplockState struct {
	// Level is the current oplock level held on this file.
	Level uint8

	// HolderFileID is the FileID of the client holding the oplock.
	// Only meaningful when Level > OplockLevelNone.
	HolderFileID [16]byte

	// HolderSessionID is the session ID of the oplock holder.
	HolderSessionID uint64

	// BreakPending indicates an oplock break is in progress.
	// When true, the oplock is being downgraded and we're waiting
	// for the client to acknowledge the break.
	BreakPending bool

	// BreakToLevel is the level we're breaking to.
	// Only meaningful when BreakPending is true.
	BreakToLevel uint8
}

// OplockManager manages oplocks and leases across all files.
//
// For backward compatibility, traditional oplocks are still stored per-path.
// SMB2.1+ leases are stored in the unified lock manager via lockStore.
//
// The manager delegates lease operations to the unified lock manager,
// enabling cross-protocol visibility (SMB leases visible to NLM).
type OplockManager struct {
	mu     sync.RWMutex
	locks  map[string]*OplockState // path -> oplock state (legacy oplocks)
	notify OplockBreakNotifier     // callback for sending break notifications

	// Unified lock manager integration
	lockStore   lock.LockStore      // For lease persistence
	leaseNotify LeaseBreakNotifier  // Lease-specific notifications
	scanner     *lock.LeaseBreakScanner // Lease break timeout scanner

	// Active break tracking (for timeout management)
	activeBreaks map[string]time.Time // leaseKeyHex -> breakStartTime

	// Session tracking for break notifications
	sessionMap map[string]uint64 // leaseKeyHex -> sessionID

	// Quick lookup cache: FileHandle -> lease EnhancedLocks
	// Populated from lock store on demand, cleared on modification
	leaseCache map[string][]*lock.EnhancedLock
	cacheValid bool
}

// OplockBreakNotifier is called when an oplock break needs to be sent to a client.
// The implementation should send an SMB2 OPLOCK_BREAK notification to the session.
type OplockBreakNotifier interface {
	// SendOplockBreak sends an oplock break notification to the client.
	// fileID identifies the file, newLevel is the level to break to.
	SendOplockBreak(sessionID uint64, fileID [16]byte, newLevel uint8) error
}

// NewOplockManager creates a new oplock manager without lock store (legacy mode).
// Use NewOplockManagerWithStore for unified lock manager integration.
func NewOplockManager() *OplockManager {
	return &OplockManager{
		locks:        make(map[string]*OplockState),
		activeBreaks: make(map[string]time.Time),
		sessionMap:   make(map[string]uint64),
		leaseCache:   make(map[string][]*lock.EnhancedLock),
	}
}

// NewOplockManagerWithStore creates an oplock manager with unified lock store integration.
// This enables SMB2.1+ lease support and cross-protocol visibility.
func NewOplockManagerWithStore(lockStore lock.LockStore) *OplockManager {
	return &OplockManager{
		locks:        make(map[string]*OplockState),
		lockStore:    lockStore,
		activeBreaks: make(map[string]time.Time),
		sessionMap:   make(map[string]uint64),
		leaseCache:   make(map[string][]*lock.EnhancedLock),
	}
}

// SetNotifier sets the callback for oplock break notifications.
func (m *OplockManager) SetNotifier(notifier OplockBreakNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify = notifier
}

// SetLeaseNotifier sets the callback for lease break notifications.
func (m *OplockManager) SetLeaseNotifier(notifier LeaseBreakNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leaseNotify = notifier
}

// SetLockStore sets the lock store for lease persistence.
func (m *OplockManager) SetLockStore(store lock.LockStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockStore = store
}

// RequestOplock attempts to grant an oplock on a file.
//
// Parameters:
//   - path: normalized, share-relative file path
//   - fileID: the SMB2 FileID for this open
//   - sessionID: the session requesting the oplock
//   - requestedLevel: the oplock level requested by the client
//
// Returns the granted oplock level (may be lower than requested if conflict exists).
func (m *OplockManager) RequestOplock(path string, fileID [16]byte, sessionID uint64, requestedLevel uint8) uint8 {
	// Don't grant oplocks for lease requests (not supported)
	if requestedLevel == OplockLevelLease {
		logger.Debug("Oplock: lease request downgraded to none", "path", path)
		return OplockLevelNone
	}

	// Don't grant oplocks for no-oplock requests
	if requestedLevel == OplockLevelNone {
		return OplockLevelNone
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.locks[path]

	// No existing oplock - grant as requested
	if existing == nil || existing.Level == OplockLevelNone {
		m.locks[path] = &OplockState{
			Level:           requestedLevel,
			HolderFileID:    fileID,
			HolderSessionID: sessionID,
		}
		logger.Debug("Oplock: granted",
			"path", path,
			"level", oplockLevelName(requestedLevel),
			"sessionID", sessionID)
		return requestedLevel
	}

	// Same session opening file again - can upgrade or maintain
	if existing.HolderSessionID == sessionID && existing.HolderFileID == fileID {
		// Same handle - keep existing oplock
		return existing.Level
	}

	// Different session or handle - need to break or deny
	if existing.Level == OplockLevelII {
		// Level II can coexist with other Level II
		if requestedLevel == OplockLevelII {
			// Multiple Level II oplocks allowed, but we only track one holder
			// In practice, Level II means "shared read caching"
			logger.Debug("Oplock: Level II coexistence",
				"path", path,
				"newSessionID", sessionID)
			return OplockLevelII
		}

		// Exclusive/Batch request conflicts with Level II - break existing
		if m.notify != nil {
			m.initiateBreak(path, existing, OplockLevelNone)
		} else {
			logger.Warn("Oplock: cannot send break notification (notifier not set)",
				"path", path,
				"conflictingLevel", oplockLevelName(existing.Level))
		}
		// Don't grant exclusive until break completes
		return OplockLevelNone
	}

	// Exclusive or Batch oplock exists - need to break
	if existing.Level == OplockLevelExclusive || existing.Level == OplockLevelBatch {
		breakTo := OplockLevelII
		if requestedLevel == OplockLevelExclusive || requestedLevel == OplockLevelBatch {
			// Exclusive request - break all the way to none
			breakTo = OplockLevelNone
		}

		if !existing.BreakPending {
			if m.notify != nil {
				m.initiateBreak(path, existing, breakTo)
			} else {
				logger.Warn("Oplock: cannot send break notification (notifier not set)",
					"path", path,
					"conflictingLevel", oplockLevelName(existing.Level))
			}
		}

		// Don't grant a new oplock until the break has completed
		// The caller can retry after the break acknowledgment
		return OplockLevelNone
	}

	return OplockLevelNone
}

// initiateBreak starts an oplock break and notifies the holder.
// Must be called with m.mu held.
func (m *OplockManager) initiateBreak(path string, state *OplockState, breakTo uint8) {
	if state.BreakPending {
		return // Already breaking
	}

	state.BreakPending = true
	state.BreakToLevel = breakTo

	logger.Debug("Oplock: initiating break",
		"path", path,
		"from", oplockLevelName(state.Level),
		"to", oplockLevelName(breakTo),
		"sessionID", state.HolderSessionID)

	// Capture values needed by the asynchronous notification to avoid races.
	// The goroutine must not access state after the mutex is released.
	holderSessionID := state.HolderSessionID
	holderFileID := state.HolderFileID
	targetLevel := breakTo
	opPath := path

	// Send break notification asynchronously
	go func() {
		if err := m.notify.SendOplockBreak(holderSessionID, holderFileID, targetLevel); err != nil {
			logger.Warn("Oplock: failed to send break notification",
				"path", opPath,
				"error", err)
		}
	}()
}

// AcknowledgeBreak handles the client's response to an oplock break.
//
// Parameters:
//   - path: the file path
//   - fileID: the FileID from the break acknowledgment
//   - newLevel: the level the client is acknowledging
//
// Returns an error if the acknowledgment is invalid.
func (m *OplockManager) AcknowledgeBreak(path string, fileID [16]byte, newLevel uint8) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.locks[path]
	if state == nil {
		return fmt.Errorf("no oplock state for path")
	}

	if state.HolderFileID != fileID {
		return fmt.Errorf("FileID mismatch in break acknowledgment")
	}

	if !state.BreakPending {
		return fmt.Errorf("no break pending")
	}

	// Client must acknowledge to the expected level or lower
	if newLevel > state.BreakToLevel {
		return fmt.Errorf("invalid break acknowledgment level: got %d, expected <= %d",
			newLevel, state.BreakToLevel)
	}

	// Update state
	state.Level = newLevel
	state.BreakPending = false
	state.BreakToLevel = 0

	if newLevel == OplockLevelNone {
		delete(m.locks, path)
	}

	logger.Debug("Oplock: break acknowledged",
		"path", path,
		"newLevel", oplockLevelName(newLevel))

	return nil
}

// ReleaseOplock releases an oplock when a file is closed.
//
// Parameters:
//   - path: the file path
//   - fileID: the FileID of the closing handle
func (m *OplockManager) ReleaseOplock(path string, fileID [16]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.locks[path]
	if state == nil {
		return
	}

	// Only release if this is the oplock holder
	if state.HolderFileID != fileID {
		return
	}

	delete(m.locks, path)
	logger.Debug("Oplock: released on close",
		"path", path,
		"level", oplockLevelName(state.Level))
}

// GetOplockLevel returns the current oplock level for a file.
func (m *OplockManager) GetOplockLevel(path string) uint8 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	state := m.locks[path]
	if state == nil {
		return OplockLevelNone
	}
	return state.Level
}

// ============================================================================
// OPLOCK_BREAK Request/Response Structures [MS-SMB2] 2.2.23, 2.2.24
// ============================================================================

// OplockBreakRequest represents an SMB2 OPLOCK_BREAK acknowledgment [MS-SMB2] 2.2.24.1.
//
// This is sent by the client in response to a server-initiated oplock break notification.
// The client acknowledges the break by specifying the new oplock level.
//
// **Wire Format (24 bytes):**
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       2     StructureSize    Always 24
//	2       1     OplockLevel      New oplock level (0x00, 0x01)
//	3       1     Reserved         Reserved (0)
//	4       4     Reserved2        Reserved (0)
//	8       16    FileId           SMB2 file identifier
type OplockBreakRequest struct {
	// OplockLevel is the new oplock level the client is acknowledging.
	// Valid values: OplockLevelNone (0x00), OplockLevelII (0x01)
	OplockLevel uint8

	// FileID is the SMB2 file identifier from the original CREATE.
	FileID [16]byte
}

// OplockBreakResponse represents an SMB2 OPLOCK_BREAK response [MS-SMB2] 2.2.25.
//
// **Wire Format (24 bytes):**
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       2     StructureSize    Always 24
//	2       1     OplockLevel      Acknowledged oplock level
//	3       1     Reserved         Reserved (0)
//	4       4     Reserved2        Reserved (0)
//	8       16    FileId           SMB2 file identifier
type OplockBreakResponse struct {
	SMBResponseBase
	OplockLevel uint8
	FileID      [16]byte
}

// DecodeOplockBreakRequest parses an OPLOCK_BREAK acknowledgment.
func DecodeOplockBreakRequest(body []byte) (*OplockBreakRequest, error) {
	if len(body) < 24 {
		return nil, fmt.Errorf("OPLOCK_BREAK request too short: %d bytes", len(body))
	}

	structSize := binary.LittleEndian.Uint16(body[0:2])
	if structSize != 24 {
		return nil, fmt.Errorf("invalid OPLOCK_BREAK structure size: %d", structSize)
	}

	oplockLevel := body[2]
	// Per MS-SMB2 2.2.24.1, valid acknowledgment levels are OplockLevelNone (0x00)
	// and OplockLevelII (0x01). Clients cannot acknowledge with Exclusive/Batch.
	if oplockLevel != OplockLevelNone && oplockLevel != OplockLevelII {
		return nil, fmt.Errorf("invalid OPLOCK_BREAK acknowledgment level: 0x%02X", oplockLevel)
	}

	req := &OplockBreakRequest{
		OplockLevel: oplockLevel,
	}
	copy(req.FileID[:], body[8:24])

	return req, nil
}

// Encode serializes the OplockBreakResponse to wire format.
func (resp *OplockBreakResponse) Encode() ([]byte, error) {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint16(buf[0:2], 24) // StructureSize
	buf[2] = resp.OplockLevel                   // OplockLevel
	buf[3] = 0                                  // Reserved
	binary.LittleEndian.PutUint32(buf[4:8], 0)  // Reserved2
	copy(buf[8:24], resp.FileID[:])             // FileId

	return buf, nil
}

// ============================================================================
// OPLOCK_BREAK Notification [MS-SMB2] 2.2.23
// ============================================================================

// OplockBreakNotification is sent by the server to notify a client that
// their oplock is being broken due to a conflicting open by another client.
//
// **Wire Format (24 bytes):**
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       2     StructureSize    Always 24
//	2       1     OplockLevel      New oplock level (level to break to)
//	3       1     Reserved         Reserved (0)
//	4       4     Reserved2        Reserved (0)
//	8       16    FileId           SMB2 file identifier
type OplockBreakNotification struct {
	OplockLevel uint8
	FileID      [16]byte
}

// Encode serializes the OplockBreakNotification to wire format.
func (n *OplockBreakNotification) Encode() ([]byte, error) {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint16(buf[0:2], 24) // StructureSize
	buf[2] = n.OplockLevel                      // OplockLevel (break to level)
	buf[3] = 0                                  // Reserved
	binary.LittleEndian.PutUint32(buf[4:8], 0)  // Reserved2
	copy(buf[8:24], n.FileID[:])                // FileId

	return buf, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// oplockLevelName returns a human-readable name for an oplock level.
func oplockLevelName(level uint8) string {
	switch level {
	case OplockLevelNone:
		return "None"
	case OplockLevelII:
		return "LevelII"
	case OplockLevelExclusive:
		return "Exclusive"
	case OplockLevelBatch:
		return "Batch"
	case OplockLevelLease:
		return "Lease"
	default:
		return fmt.Sprintf("Unknown(0x%02X)", level)
	}
}

// BuildOplockPath constructs a normalized oplock path from share name and file path.
// This ensures consistent path construction across all handlers.
func BuildOplockPath(shareName, filePath string) string {
	return path.Join(shareName, filePath)
}

// ============================================================================
// Lease Break Scanner Integration
// ============================================================================

// StartScanner starts the lease break timeout scanner.
// The scanner runs in the background and force-revokes leases that don't
// acknowledge breaks within the timeout period.
func (m *OplockManager) StartScanner() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lockStore == nil {
		logger.Warn("OplockManager: cannot start scanner without lock store")
		return
	}

	if m.scanner == nil {
		m.scanner = lock.NewLeaseBreakScanner(m.lockStore, m, 0)
	}
	m.scanner.Start()

	logger.Debug("OplockManager: lease break scanner started")
}

// StopScanner stops the lease break timeout scanner.
func (m *OplockManager) StopScanner() {
	m.mu.Lock()
	scanner := m.scanner
	m.mu.Unlock()

	if scanner != nil {
		scanner.Stop()
		logger.Debug("OplockManager: lease break scanner stopped")
	}
}

// OnLeaseBreakTimeout implements lock.LeaseBreakCallback.
// Called by the scanner when a lease break times out without acknowledgment.
func (m *OplockManager) OnLeaseBreakTimeout(leaseKey [16]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	keyHex := fmt.Sprintf("%x", leaseKey)

	// Remove from active breaks
	delete(m.activeBreaks, keyHex)

	// Remove from session map
	delete(m.sessionMap, keyHex)

	// Invalidate cache
	m.invalidateCache()

	logger.Debug("OplockManager: lease break timeout force-revoked",
		"leaseKey", keyHex)
}

// ============================================================================
// Cross-Protocol Integration
// ============================================================================

// CheckAndBreakForWrite checks for SMB leases that conflict with a write operation
// and initiates breaks as needed. Called by NFS/NLM handlers before writes.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file being written to
//
// Returns nil if no conflict, or blocks until breaks are acknowledged/timeout.
func (m *OplockManager) CheckAndBreakForWrite(ctx context.Context, fileHandle lock.FileHandle) error {
	m.mu.Lock()
	defer m.mu.Unlock()

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
		return nil // Don't block write on query failure
	}

	for _, pl := range leases {
		if len(pl.LeaseKey) != 16 {
			continue
		}

		el := lock.FromPersistedLock(pl)
		if el.Lease == nil {
			continue
		}

		// Write conflicts with Write lease (client has cached writes)
		// and Read lease (cached reads become stale)
		if el.Lease.HasWrite() || el.Lease.HasRead() {
			if !el.Lease.Breaking {
				// Break to None for write conflict
				m.initiateLeaseBreak(el, lock.LeaseStateNone)
			}
		}
	}

	return nil
}

// CheckAndBreakForRead checks for SMB leases that conflict with a read operation
// and initiates breaks as needed. Called by NFS/NLM handlers before reads.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileHandle: The file being read from
//
// Returns nil if no conflict.
func (m *OplockManager) CheckAndBreakForRead(ctx context.Context, fileHandle lock.FileHandle) error {
	m.mu.Lock()
	defer m.mu.Unlock()

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
		return nil // Don't block read on query failure
	}

	for _, pl := range leases {
		if len(pl.LeaseKey) != 16 {
			continue
		}

		el := lock.FromPersistedLock(pl)
		if el.Lease == nil {
			continue
		}

		// Read only conflicts with Write lease (dirty data must be flushed)
		// Read leases can coexist with reads
		if el.Lease.HasWrite() && !el.Lease.Breaking {
			// Break Write, keep Read+Handle
			m.initiateLeaseBreak(el, lock.LeaseStateRead|lock.LeaseStateHandle)
		}
	}

	return nil
}
