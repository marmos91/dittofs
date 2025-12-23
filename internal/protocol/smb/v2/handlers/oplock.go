// Package handlers provides SMB2 command handlers and session management.
//
// This file implements SMB2 opportunistic lock (oplock) management [MS-SMB2] 2.2.23, 2.2.24.
// Oplocks allow clients to cache file data aggressively for better performance.
package handlers

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
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

// OplockManager manages oplocks across all files.
//
// Oplocks are stored per-path (normalized, share-relative path).
// The manager handles oplock grants, conflicts, and breaks.
type OplockManager struct {
	mu     sync.RWMutex
	locks  map[string]*OplockState // path -> oplock state
	notify OplockBreakNotifier     // callback for sending break notifications
}

// OplockBreakNotifier is called when an oplock break needs to be sent to a client.
// The implementation should send an SMB2 OPLOCK_BREAK notification to the session.
type OplockBreakNotifier interface {
	// SendOplockBreak sends an oplock break notification to the client.
	// fileID identifies the file, newLevel is the level to break to.
	SendOplockBreak(sessionID uint64, fileID [16]byte, newLevel uint8) error
}

// NewOplockManager creates a new oplock manager.
func NewOplockManager() *OplockManager {
	return &OplockManager{
		locks: make(map[string]*OplockState),
	}
}

// SetNotifier sets the callback for oplock break notifications.
func (m *OplockManager) SetNotifier(notifier OplockBreakNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify = notifier
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

		if m.notify != nil && !existing.BreakPending {
			m.initiateBreak(path, existing, breakTo)
		}

		// Grant Level II if breaking to II, otherwise none
		if breakTo == OplockLevelII {
			return OplockLevelII
		}
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

	// Send break notification asynchronously
	go func() {
		if err := m.notify.SendOplockBreak(state.HolderSessionID, state.HolderFileID, breakTo); err != nil {
			logger.Warn("Oplock: failed to send break notification",
				"path", path,
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

// CheckConflict checks if opening a file would conflict with existing oplocks.
// This is used to determine if we need to wait for an oplock break.
//
// Parameters:
//   - path: the file path
//   - desiredAccess: the access rights requested
//   - shareAccess: the sharing mode
//
// Returns (needsBreak bool, breakTo uint8).
func (m *OplockManager) CheckConflict(path string, desiredAccess uint32, shareAccess uint32) (bool, uint8) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	state := m.locks[path]
	if state == nil || state.Level == OplockLevelNone {
		return false, OplockLevelNone
	}

	// Write access always conflicts with exclusive oplocks
	if desiredAccess&0x2 != 0 { // FILE_WRITE_DATA
		if state.Level == OplockLevelExclusive || state.Level == OplockLevelBatch {
			return true, OplockLevelNone
		}
	}

	// Level II allows shared read, so no conflict for read-only
	if state.Level == OplockLevelII {
		return false, OplockLevelNone
	}

	// Exclusive/Batch - any open that doesn't share appropriately conflicts
	return true, OplockLevelII
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

	req := &OplockBreakRequest{
		OplockLevel: body[2],
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
