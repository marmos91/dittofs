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

// OpenFileInfo is a summary of an open file for debugging.
type OpenFileInfo struct {
	FileID    string
	SessionID uint64
	Path      string
	ShareName string
	OpenTime  time.Time
	IsDir     bool
	IsPipe    bool
}

// LeaseInfo is a summary of an active lease for debugging.
type LeaseInfo struct {
	LeaseKey  string
	SessionID uint64
	ShareName string
}

// TreeInfo is a summary of a tree connection for debugging.
type TreeInfo struct {
	TreeID    uint32
	SessionID uint64
	ShareName string
}

// NotifyInfo is a summary of a pending notify watcher for debugging.
type NotifyInfo struct {
	FileID    string
	SessionID uint64
	WatchPath string
	ShareName string
}

// StateDump is a detailed dump of all shared Handler state.
type StateDump struct {
	Snapshot StateSnapshot
	Files    []OpenFileInfo
	Leases   []LeaseInfo
	Trees    []TreeInfo
	Notifies []NotifyInfo
}

// DumpState returns a detailed dump of all shared Handler state.
// This is intended for debugging and testing, not for hot-path use.
func (h *Handler) DumpState() StateDump {
	dump := StateDump{
		Snapshot: h.TakeStateSnapshot(),
	}

	// Collect open files
	h.files.Range(func(key, value any) bool {
		f := value.(*OpenFile)
		dump.Files = append(dump.Files, OpenFileInfo{
			FileID:    fmt.Sprintf("%x", f.FileID),
			SessionID: f.SessionID,
			Path:      f.Path,
			ShareName: f.ShareName,
			OpenTime:  f.OpenTime,
			IsDir:     f.IsDirectory,
			IsPipe:    f.IsPipe,
		})
		return true
	})

	// Collect leases from LeaseManager
	if h.LeaseManager != nil {
		h.LeaseManager.RangeLeases(func(leaseKeyHex string, sessionID uint64, shareName string) bool {
			dump.Leases = append(dump.Leases, LeaseInfo{
				LeaseKey:  leaseKeyHex,
				SessionID: sessionID,
				ShareName: shareName,
			})
			return true
		})
	}

	// Collect trees
	h.trees.Range(func(key, value any) bool {
		t := value.(*TreeConnection)
		dump.Trees = append(dump.Trees, TreeInfo{
			TreeID:    t.TreeID,
			SessionID: t.SessionID,
			ShareName: t.ShareName,
		})
		return true
	})

	// Collect notify watchers
	if h.NotifyRegistry != nil {
		h.NotifyRegistry.RangeWatchers(func(n *PendingNotify) bool {
			dump.Notifies = append(dump.Notifies, NotifyInfo{
				FileID:    fmt.Sprintf("%x", n.FileID),
				SessionID: n.SessionID,
				WatchPath: n.WatchPath,
				ShareName: n.ShareName,
			})
			return true
		})
	}

	return dump
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

	// Check pending auth
	if _, ok := h.pendingAuth.Load(sessionID); ok {
		leaked++
		logger.Warn("LEAKED pending auth after session cleanup",
			"sessionID", sessionID,
		)
	}

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
