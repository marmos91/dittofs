package metadata

// ============================================================================
// Re-exported lock types from lock package for backward compatibility
//
// DEPRECATED: Import directly from github.com/marmos91/dittofs/pkg/metadata/lock
// ============================================================================

import (
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Lock Types
// ============================================================================

// LockType is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockType = lock.LockType

// Lock type constants re-exported for backward compatibility.
const (
	// LockTypeShared is re-exported from the lock package.
	LockTypeShared = lock.LockTypeShared
	// LockTypeExclusive is re-exported from the lock package.
	LockTypeExclusive = lock.LockTypeExclusive
)

// AccessMode is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type AccessMode = lock.AccessMode

// Share reservation constants re-exported for backward compatibility.
const (
	// AccessModeNone is re-exported from the lock package.
	AccessModeNone = lock.AccessModeNone
	// AccessModeDenyRead is re-exported from the lock package.
	AccessModeDenyRead = lock.AccessModeDenyRead
	// AccessModeDenyWrite is re-exported from the lock package.
	AccessModeDenyWrite = lock.AccessModeDenyWrite
	// AccessModeDenyAll is re-exported from the lock package.
	AccessModeDenyAll = lock.AccessModeDenyAll
)

// LockOwner is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockOwner = lock.LockOwner

// UnifiedLock is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type UnifiedLock = lock.UnifiedLock

// FileLock is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type FileLock = lock.FileLock

// ============================================================================
// Lock Manager Types
// ============================================================================

// LockManager is re-exported from the lock package as Manager.
// Deprecated: Import Manager from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockManager = lock.Manager

// NewLockManager creates a new lock manager.
// Deprecated: Use lock.NewManager() directly.
func NewLockManager() *LockManager {
	return lock.NewManager()
}

// ============================================================================
// Configuration Types
// ============================================================================

// LockConfig is re-exported from the lock package as Config.
// Deprecated: Import Config from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockConfig = lock.Config

// DefaultLockConfig returns default lock configuration.
// Deprecated: Use lock.DefaultConfig() directly.
func DefaultLockConfig() LockConfig {
	return lock.DefaultConfig()
}

// LockLimits is re-exported from the lock package as Limits.
// Deprecated: Import Limits from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockLimits = lock.Limits

// NewLockLimits creates a new limits tracker.
// Deprecated: Use lock.NewLimits() directly.
func NewLockLimits() *LockLimits {
	return lock.NewLimits()
}

// LockStats is re-exported from the lock package as Stats.
// Deprecated: Import Stats from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockStats = lock.Stats

// ============================================================================
// Grace Period Types
// ============================================================================

// GraceState is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type GraceState = lock.GraceState

// Grace state constants re-exported for backward compatibility.
const (
	// GraceStateNormal is re-exported from the lock package.
	GraceStateNormal = lock.GraceStateNormal
	// GraceStateActive is re-exported from the lock package.
	GraceStateActive = lock.GraceStateActive
)

// GracePeriodManager is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type GracePeriodManager = lock.GracePeriodManager

// NewGracePeriodManager creates a new grace period manager.
// Deprecated: Use lock.NewGracePeriodManager() directly.
var NewGracePeriodManager = lock.NewGracePeriodManager

// LockOperation is re-exported from the lock package as Operation.
// Deprecated: Import Operation from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockOperation = lock.Operation

// ============================================================================
// Connection Tracking Types
// ============================================================================

// ConnectionTracker is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type ConnectionTracker = lock.ConnectionTracker

// NewConnectionTracker creates a new connection tracker.
// Deprecated: Use lock.NewConnectionTracker() directly.
var NewConnectionTracker = lock.NewConnectionTracker

// ConnectionTrackerConfig is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type ConnectionTrackerConfig = lock.ConnectionTrackerConfig

// DefaultConnectionTrackerConfig returns default connection tracker config.
// Deprecated: Use lock.DefaultConnectionTrackerConfig() directly.
var DefaultConnectionTrackerConfig = lock.DefaultConnectionTrackerConfig

// ClientRegistration is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type ClientRegistration = lock.ClientRegistration

// ============================================================================
// Metrics Types
// ============================================================================

// LockMetrics is re-exported from the lock package as Metrics.
// Deprecated: Import Metrics from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockMetrics = lock.Metrics

// NewLockMetrics creates new lock metrics.
// Deprecated: Use lock.NewMetrics() directly.
var NewLockMetrics = lock.NewMetrics

// Metrics label constants re-exported for backward compatibility.
const (
	LabelShare     = lock.LabelShare
	LabelType      = lock.LabelType
	LabelStatus    = lock.LabelStatus
	LabelReason    = lock.LabelReason
	LabelAdapter   = lock.LabelAdapter
	LabelEvent     = lock.LabelEvent
	LabelLimitType = lock.LabelLimitType
)

// Status constants re-exported for backward compatibility.
const (
	StatusGranted  = lock.StatusGranted
	StatusDenied   = lock.StatusDenied
	StatusDeadlock = lock.StatusDeadlock
)

// Reason constants re-exported for backward compatibility.
const (
	ReasonExplicit     = lock.ReasonExplicit
	ReasonTimeout      = lock.ReasonTimeout
	ReasonDisconnect   = lock.ReasonDisconnect
	ReasonGraceExpired = lock.ReasonGraceExpired
)

// ============================================================================
// Deadlock Detection Types
// ============================================================================

// WaitForGraph is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type WaitForGraph = lock.WaitForGraph

// NewWaitForGraph creates a new wait-for graph.
// Deprecated: Use lock.NewWaitForGraph() directly.
var NewWaitForGraph = lock.NewWaitForGraph

// ============================================================================
// Persistence Types
// ============================================================================

// LockStore is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockStore = lock.LockStore

// PersistedLock is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type PersistedLock = lock.PersistedLock

// LockQuery is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockQuery = lock.LockQuery

// ToPersistedLock converts an UnifiedLock to a PersistedLock.
// Deprecated: Use lock.ToPersistedLock() directly.
var ToPersistedLock = lock.ToPersistedLock

// FromPersistedLock converts a PersistedLock to an UnifiedLock.
// Deprecated: Use lock.FromPersistedLock() directly.
var FromPersistedLock = lock.FromPersistedLock

// ============================================================================
// Utility Functions
// ============================================================================

// RangesOverlap checks if two byte ranges overlap.
// Deprecated: Use lock.RangesOverlap() directly.
var RangesOverlap = lock.RangesOverlap

// IsLockConflicting checks if two locks conflict.
// Deprecated: Use lock.IsLockConflicting() directly.
var IsLockConflicting = lock.IsLockConflicting

// IsUnifiedLockConflicting checks if two unified locks conflict.
// Deprecated: Use lock.IsUnifiedLockConflicting() directly.
var IsUnifiedLockConflicting = lock.IsUnifiedLockConflicting

// CheckIOConflict checks if an I/O operation conflicts with a lock.
// Deprecated: Use lock.CheckIOConflict() directly.
var CheckIOConflict = lock.CheckIOConflict

// SplitLock splits an existing lock when a portion is unlocked.
// Deprecated: Use lock.SplitLock() directly.
var SplitLock = lock.SplitLock

// MergeLocks coalesces adjacent or overlapping locks.
// Deprecated: Use lock.MergeLocks() directly.
var MergeLocks = lock.MergeLocks

// NewUnifiedLock creates a new UnifiedLock with a generated UUID.
// Deprecated: Use lock.NewUnifiedLock() directly.
var NewUnifiedLock = lock.NewUnifiedLock

// OpLock is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type OpLock = lock.OpLock

// OpLockBreakScanner is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type OpLockBreakScanner = lock.OpLockBreakScanner

// OpLockBreakCallback is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type OpLockBreakCallback = lock.OpLockBreakCallback

// OpLocksConflict checks if two OpLock leases conflict.
// Deprecated: Use lock.OpLocksConflict() directly.
var OpLocksConflict = lock.OpLocksConflict

// NLMHolderInfo is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type NLMHolderInfo = lock.NLMHolderInfo

// TranslateToNLMHolder translates an SMB lease to NLM holder format.
// Deprecated: Use lock.TranslateToNLMHolder() directly.
var TranslateToNLMHolder = lock.TranslateToNLMHolder
