package metadata

// ============================================================================
// Public lock surface of package metadata
// ============================================================================
//
// The names below are package metadata's lock API. The NFS and SMB adapters,
// the control-plane runtime and the metadata store backends refer to lock
// state as metadata.FileLock, metadata.PersistedLock, metadata.LockQuery and
// the rest.
//
// The implementations live in pkg/metadata/lock because that package may not
// import metadata — metadata imports it, so the dependency only runs one way.
// That lets the lock manager and the backends that persist locks share one set
// of types without a cycle, while the aliases here keep the API reachable from
// the package that owns it.
//
// These are aliases, not distinct types, so a lock built through either
// spelling satisfies both. Removing or renaming a name here breaks every
// caller of the metadata package.
// ============================================================================

import (
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Lock Types
// ============================================================================

// LockType is re-exported from the lock package.
type LockType = lock.LockType

// The lock-type constants callers compare LockType against.
const (
	// LockTypeShared is re-exported from the lock package.
	LockTypeShared = lock.LockTypeShared
	// LockTypeExclusive is re-exported from the lock package.
	LockTypeExclusive = lock.LockTypeExclusive
)

// AccessMode is re-exported from the lock package.
type AccessMode = lock.AccessMode

// The share-reservation constants callers compare AccessMode against.
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
type LockOwner = lock.LockOwner

// UnifiedLock is re-exported from the lock package.
type UnifiedLock = lock.UnifiedLock

// FileLock is re-exported from the lock package.
type FileLock = lock.FileLock

// ============================================================================
// Lock Manager Types
// ============================================================================

// LockManager is re-exported from the lock package as Manager.
type LockManager = lock.Manager

// NewLockManager creates a new lock manager.
func NewLockManager() *LockManager {
	return lock.NewManager()
}

// ============================================================================
// Configuration Types
// ============================================================================

// LockConfig is re-exported from the lock package as Config.
type LockConfig = lock.Config

// DefaultLockConfig returns default lock configuration.
func DefaultLockConfig() LockConfig {
	return lock.DefaultConfig()
}

// LockLimits is re-exported from the lock package as Limits.
type LockLimits = lock.Limits

// NewLockLimits creates a new limits tracker.
func NewLockLimits() *LockLimits {
	return lock.NewLimits()
}

// LockStats is re-exported from the lock package as Stats.
type LockStats = lock.Stats

// ============================================================================
// Grace Period Types
// ============================================================================

// GraceState is re-exported from the lock package.
type GraceState = lock.GraceState

// The grace states callers compare GraceState against.
const (
	// GraceStateNormal is re-exported from the lock package.
	GraceStateNormal = lock.GraceStateNormal
	// GraceStateActive is re-exported from the lock package.
	GraceStateActive = lock.GraceStateActive
)

// GracePeriodManager is re-exported from the lock package.
type GracePeriodManager = lock.GracePeriodManager

// LockOperation is re-exported from the lock package as Operation.
type LockOperation = lock.Operation

// ============================================================================
// Connection Tracking Types
// ============================================================================

// ConnectionTracker is re-exported from the lock package.
type ConnectionTracker = lock.ConnectionTracker

// ConnectionTrackerConfig is re-exported from the lock package.
type ConnectionTrackerConfig = lock.ConnectionTrackerConfig

// ClientRegistration is re-exported from the lock package.
type ClientRegistration = lock.ClientRegistration

// ============================================================================
// Deadlock Detection Types
// ============================================================================

// WaitForGraph is re-exported from the lock package.
type WaitForGraph = lock.WaitForGraph

// ============================================================================
// Persistence Types
// ============================================================================

// LockStore is re-exported from the lock package.
type LockStore = lock.LockStore

// PersistedLock is re-exported from the lock package.
type PersistedLock = lock.PersistedLock

// LockQuery is re-exported from the lock package.
type LockQuery = lock.LockQuery

// ============================================================================
// Utility Functions
// ============================================================================

// OpLock is re-exported from the lock package.
type OpLock = lock.OpLock

// OpLockBreakScanner is re-exported from the lock package.
type OpLockBreakScanner = lock.OpLockBreakScanner

// OpLockBreakCallback is re-exported from the lock package.
type OpLockBreakCallback = lock.OpLockBreakCallback

// NLMHolderInfo is re-exported from the lock package.
type NLMHolderInfo = lock.NLMHolderInfo
