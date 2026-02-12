package lock

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ============================================================================
// Lock Configuration
// ============================================================================

// Config contains configuration settings for the lock manager.
//
// These settings control lock limits, timeouts, and behavior across
// all protocols (NLM, SMB, NFSv4).
type Config struct {
	// MaxLocksPerFile is the maximum number of locks allowed on a single file.
	// Prevents a single file from exhausting lock table resources.
	// Default: 1000
	MaxLocksPerFile int `mapstructure:"max_locks_per_file" yaml:"max_locks_per_file"`

	// MaxLocksPerClient is the maximum number of locks a single client can hold.
	// Prevents a single client from exhausting lock table resources.
	// Default: 10000
	MaxLocksPerClient int `mapstructure:"max_locks_per_client" yaml:"max_locks_per_client"`

	// MaxTotalLocks is the maximum total locks across all files and clients.
	// Provides a hard ceiling on lock manager memory usage.
	// Default: 100000
	MaxTotalLocks int `mapstructure:"max_total_locks" yaml:"max_total_locks"`

	// BlockingTimeout is the server-side timeout for blocking lock requests.
	// After this duration, the server will return NLM_LCK_DENIED_NOLOCKS or
	// similar to the client. The client may retry.
	// Default: 60s
	BlockingTimeout time.Duration `mapstructure:"blocking_timeout" yaml:"blocking_timeout"`

	// GracePeriodDuration is the duration of the grace period after server restart.
	// During this time, only lock reclaims are allowed (new locks are denied).
	// Default: 90s (NFS spec recommends 90 seconds minimum)
	GracePeriodDuration time.Duration `mapstructure:"grace_period" yaml:"grace_period"`

	// MandatoryLocking controls whether locks are mandatory or advisory.
	// - false (default): Advisory locks - the lock manager tracks locks but
	//   I/O operations are not blocked by locks (only lock operations check locks)
	// - true: Mandatory locks - I/O operations check and are blocked by locks
	// Advisory locking is more common and has better performance.
	// Default: false
	MandatoryLocking bool `mapstructure:"mandatory_locking" yaml:"mandatory_locking"`
}

// DefaultConfig returns a Config with sensible defaults.
//
// These defaults are based on common production deployments and NFS/SMB
// protocol recommendations.
func DefaultConfig() Config {
	return Config{
		MaxLocksPerFile:     1000,
		MaxLocksPerClient:   10000,
		MaxTotalLocks:       100000,
		BlockingTimeout:     60 * time.Second,
		GracePeriodDuration: 90 * time.Second,
		MandatoryLocking:    false,
	}
}

// ============================================================================
// Lock Limits Tracking
// ============================================================================

// Limits tracks current lock usage for limit enforcement.
//
// Thread Safety:
// Limits is safe for concurrent use by multiple goroutines.
type Limits struct {
	mu sync.RWMutex

	// locksByFile tracks lock count per file (fileHandle -> count)
	locksByFile map[string]int

	// locksByClient tracks lock count per client (clientID -> count)
	locksByClient map[string]int

	// totalLocks is the current total lock count
	totalLocks int
}

// NewLimits creates a new Limits tracker.
func NewLimits() *Limits {
	return &Limits{
		locksByFile:   make(map[string]int),
		locksByClient: make(map[string]int),
	}
}

// CheckLimits verifies that acquiring a new lock would not exceed any limits.
//
// Parameters:
//   - config: The lock configuration containing limits
//   - fileHandle: The file handle for the lock
//   - clientID: The client ID acquiring the lock
//
// Returns:
//   - nil if limits would not be exceeded
//   - ErrLockLimitExceeded if any limit would be exceeded
func (ll *Limits) CheckLimits(config Config, fileHandle, clientID string) error {
	ll.mu.RLock()
	defer ll.mu.RUnlock()

	// Check file limit
	if config.MaxLocksPerFile > 0 {
		current := ll.locksByFile[fileHandle]
		if current >= config.MaxLocksPerFile {
			return NewLockLimitExceededError("per-file", current, config.MaxLocksPerFile)
		}
	}

	// Check client limit
	if config.MaxLocksPerClient > 0 {
		current := ll.locksByClient[clientID]
		if current >= config.MaxLocksPerClient {
			return NewLockLimitExceededError("per-client", current, config.MaxLocksPerClient)
		}
	}

	// Check total limit
	if config.MaxTotalLocks > 0 {
		if ll.totalLocks >= config.MaxTotalLocks {
			return NewLockLimitExceededError("total", ll.totalLocks, config.MaxTotalLocks)
		}
	}

	return nil
}

// IncrementCounts updates counters after successfully acquiring a lock.
//
// Call this AFTER the lock has been successfully acquired.
//
// Parameters:
//   - fileHandle: The file handle the lock was acquired on
//   - clientID: The client ID that acquired the lock
func (ll *Limits) IncrementCounts(fileHandle, clientID string) {
	ll.mu.Lock()
	defer ll.mu.Unlock()

	ll.locksByFile[fileHandle]++
	ll.locksByClient[clientID]++
	ll.totalLocks++
}

// DecrementCounts updates counters after releasing a lock.
//
// Call this AFTER the lock has been successfully released.
//
// Parameters:
//   - fileHandle: The file handle the lock was released from
//   - clientID: The client ID that released the lock
func (ll *Limits) DecrementCounts(fileHandle, clientID string) {
	ll.mu.Lock()
	defer ll.mu.Unlock()

	// Decrement file count (don't go below 0)
	if ll.locksByFile[fileHandle] > 0 {
		ll.locksByFile[fileHandle]--
		if ll.locksByFile[fileHandle] == 0 {
			delete(ll.locksByFile, fileHandle)
		}
	}

	// Decrement client count (don't go below 0)
	if ll.locksByClient[clientID] > 0 {
		ll.locksByClient[clientID]--
		if ll.locksByClient[clientID] == 0 {
			delete(ll.locksByClient, clientID)
		}
	}

	// Decrement total (don't go below 0)
	if ll.totalLocks > 0 {
		ll.totalLocks--
	}
}

// DecrementCountsN updates counters after releasing multiple locks at once.
//
// Call this when releasing all locks for a file or all locks for a client.
//
// Parameters:
//   - fileHandle: The file handle (can be empty if not file-specific)
//   - clientID: The client ID (can be empty if not client-specific)
//   - count: Number of locks being released
func (ll *Limits) DecrementCountsN(fileHandle, clientID string, count int) {
	if count <= 0 {
		return
	}

	ll.mu.Lock()
	defer ll.mu.Unlock()

	// Decrement file count
	if fileHandle != "" && ll.locksByFile[fileHandle] > 0 {
		ll.locksByFile[fileHandle] -= count
		if ll.locksByFile[fileHandle] <= 0 {
			delete(ll.locksByFile, fileHandle)
		}
	}

	// Decrement client count
	if clientID != "" && ll.locksByClient[clientID] > 0 {
		ll.locksByClient[clientID] -= count
		if ll.locksByClient[clientID] <= 0 {
			delete(ll.locksByClient, clientID)
		}
	}

	// Decrement total
	ll.totalLocks -= count
	if ll.totalLocks < 0 {
		ll.totalLocks = 0
	}
}

// ============================================================================
// Lock Statistics
// ============================================================================

// Stats contains current lock usage statistics.
type Stats struct {
	// TotalLocks is the total number of active locks
	TotalLocks int

	// UniqueFiles is the number of files with at least one lock
	UniqueFiles int

	// UniqueClients is the number of clients with at least one lock
	UniqueClients int

	// MaxLocksOnFile is the highest lock count on any single file
	MaxLocksOnFile int

	// MaxLocksForClient is the highest lock count for any single client
	MaxLocksForClient int
}

// GetStats returns current lock usage statistics.
//
// This is useful for monitoring and debugging.
func (ll *Limits) GetStats() Stats {
	ll.mu.RLock()
	defer ll.mu.RUnlock()

	stats := Stats{
		TotalLocks:    ll.totalLocks,
		UniqueFiles:   len(ll.locksByFile),
		UniqueClients: len(ll.locksByClient),
	}

	// Find max locks per file
	for _, count := range ll.locksByFile {
		if count > stats.MaxLocksOnFile {
			stats.MaxLocksOnFile = count
		}
	}

	// Find max locks per client
	for _, count := range ll.locksByClient {
		if count > stats.MaxLocksForClient {
			stats.MaxLocksForClient = count
		}
	}

	return stats
}

// GetFileCount returns the current lock count for a specific file.
func (ll *Limits) GetFileCount(fileHandle string) int {
	ll.mu.RLock()
	defer ll.mu.RUnlock()
	return ll.locksByFile[fileHandle]
}

// GetClientCount returns the current lock count for a specific client.
func (ll *Limits) GetClientCount(clientID string) int {
	ll.mu.RLock()
	defer ll.mu.RUnlock()
	return ll.locksByClient[clientID]
}

// GetTotalCount returns the current total lock count.
func (ll *Limits) GetTotalCount() int {
	ll.mu.RLock()
	defer ll.mu.RUnlock()
	return ll.totalLocks
}

// Reset clears all lock counts (useful for testing).
func (ll *Limits) Reset() {
	ll.mu.Lock()
	defer ll.mu.Unlock()

	ll.locksByFile = make(map[string]int)
	ll.locksByClient = make(map[string]int)
	ll.totalLocks = 0
}

// Ensure errors package is used
var _ = errors.ErrLockLimitExceeded
