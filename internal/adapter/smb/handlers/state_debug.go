package handlers

import (
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// StateSnapshot is a point-in-time summary of all shared Handler state.
// Used for leak detection instrumentation at session lifecycle boundaries.
type StateSnapshot struct {
	OpenFiles      int
	Sessions       int // excludes the anonymous session (ID 0)
	Trees          int
	PendingAuths   int
	PendingLocks   int
	Leases         int
	NotifyWatchers int
	Timestamp      time.Time
}

// String formats the snapshot as a compact summary.
func (s StateSnapshot) String() string {
	return fmt.Sprintf("files=%d sessions=%d trees=%d pendingAuth=%d pendingLocks=%d leases=%d notifies=%d",
		s.OpenFiles, s.Sessions, s.Trees, s.PendingAuths, s.PendingLocks, s.Leases, s.NotifyWatchers)
}

// IsClean returns true if all counters are zero (no residual state).
func (s StateSnapshot) IsClean() bool {
	return s.OpenFiles == 0 && s.Sessions == 0 && s.Trees == 0 &&
		s.PendingAuths == 0 && s.PendingLocks == 0 && s.Leases == 0 &&
		s.NotifyWatchers == 0
}

// TakeStateSnapshot captures a point-in-time count of all shared Handler state.
// This iterates sync.Maps (O(n)) but is acceptable for debug instrumentation
// at infrequent lifecycle boundaries (session setup, cleanup, connection close).
func (h *Handler) TakeStateSnapshot() StateSnapshot {
	snap := StateSnapshot{Timestamp: time.Now()}

	snap.OpenFiles = countSyncMap(&h.files)
	snap.Trees = countSyncMap(&h.trees)
	snap.PendingAuths = countSyncMap(&h.pendingAuth)
	snap.PendingLocks = countSyncMap(&h.pendingLocks)

	// Session count (exclude anonymous session ID 0)
	h.SessionManager.RangeSessions(func(sessionID uint64, _ any) bool {
		if sessionID != 0 {
			snap.Sessions++
		}
		return true
	})

	if h.LeaseManager != nil {
		snap.Leases = h.LeaseManager.LeaseCount()
	}
	if h.NotifyRegistry != nil {
		snap.NotifyWatchers = h.NotifyRegistry.WatcherCount()
	}

	return snap
}

// LogStateSnapshot logs a state snapshot at the given label.
// Uses Debug level for normal lifecycle events.
// Guarded by logger.IsDebugEnabled() to avoid O(n) sync.Map iteration
// when the log level is above DEBUG.
func (h *Handler) LogStateSnapshot(label string, sessionID uint64) {
	if !logger.IsDebugEnabled() {
		return
	}
	snap := h.TakeStateSnapshot()
	logger.Debug(label,
		"sessionID", sessionID,
		"state", snap.String(),
	)
}

// AuditSessionCleanup scans all shared state maps for any items still
// belonging to the given sessionID. If any are found, they are logged
// at WARN level as leaked state. This is the key leak detection mechanism.
//
// Call this AFTER CleanupSession has completed all cleanup steps.
// Returns the total number of leaked items found.
func (h *Handler) AuditSessionCleanup(sessionID uint64) int {
	leaked := 0

	// Check open files
	h.files.Range(func(key, value any) bool {
		f := value.(*OpenFile)
		if f.SessionID == sessionID {
			leaked++
			logger.Warn("LEAKED open file after session cleanup",
				"sessionID", sessionID,
				"fileID", fmt.Sprintf("%x", f.FileID),
				"path", f.Path,
				"shareName", f.ShareName,
				"openTime", f.OpenTime,
			)
		}
		return true
	})

	// Check trees
	h.trees.Range(func(key, value any) bool {
		t := value.(*TreeConnection)
		if t.SessionID == sessionID {
			leaked++
			logger.Warn("LEAKED tree connection after session cleanup",
				"sessionID", sessionID,
				"treeID", t.TreeID,
				"shareName", t.ShareName,
			)
		}
		return true
	})

	// Check pending auth (any channel for this session)
	h.pendingAuth.Range(func(k, _ any) bool {
		if key, ok := k.(pendingAuthKey); ok && key.SessionID == sessionID {
			leaked++
			logger.Warn("LEAKED pending auth after session cleanup",
				"sessionID", sessionID,
				"connID", key.ConnID,
			)
		}
		return true
	})

	// Check sessions in SessionManager
	if _, ok := h.SessionManager.GetSession(sessionID); ok {
		leaked++
		logger.Warn("LEAKED session in SessionManager after cleanup",
			"sessionID", sessionID,
		)
	}

	// Check leases
	if h.LeaseManager != nil {
		h.LeaseManager.RangeLeases(func(leaseKeyHex string, sid uint64, shareName string) bool {
			if sid == sessionID {
				leaked++
				logger.Warn("LEAKED lease after session cleanup",
					"sessionID", sessionID,
					"leaseKey", leaseKeyHex,
					"shareName", shareName,
				)
			}
			return true
		})
	}

	// Check notify watchers
	if h.NotifyRegistry != nil {
		h.NotifyRegistry.RangeWatchers(func(n *PendingNotify) bool {
			if n.SessionID == sessionID {
				leaked++
				logger.Warn("LEAKED notify watcher after session cleanup",
					"sessionID", sessionID,
					"fileID", fmt.Sprintf("%x", n.FileID),
					"watchPath", n.WatchPath,
					"shareName", n.ShareName,
				)
			}
			return true
		})
	}

	return leaked
}

// countSyncMap counts entries in a sync.Map. O(n) but acceptable for
// infrequent debug instrumentation.
func countSyncMap(m *sync.Map) int {
	count := 0
	m.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
