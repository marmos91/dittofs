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

// LockOperation is re-exported from the lock package as Operation.
// Deprecated: Import Operation from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockOperation = lock.Operation

// ============================================================================
// Connection Tracking Types
// ============================================================================

// ConnectionTracker is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type ConnectionTracker = lock.ConnectionTracker

// ConnectionTrackerConfig is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type ConnectionTrackerConfig = lock.ConnectionTrackerConfig

// ClientRegistration is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type ClientRegistration = lock.ClientRegistration

// ============================================================================
// Deadlock Detection Types
// ============================================================================

// WaitForGraph is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type WaitForGraph = lock.WaitForGraph

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

// ============================================================================
// Utility Functions
// ============================================================================

// OpLock is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type OpLock = lock.OpLock

// OpLockBreakScanner is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type OpLockBreakScanner = lock.OpLockBreakScanner

// OpLockBreakCallback is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type OpLockBreakCallback = lock.OpLockBreakCallback

// NLMHolderInfo is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type NLMHolderInfo = lock.NLMHolderInfo
