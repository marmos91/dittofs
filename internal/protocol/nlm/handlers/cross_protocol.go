// Package handlers provides cross-protocol integration helpers for NLM handlers.
//
// This file contains helpers for NLM handlers to interact with SMB leases:
//   - Wait for SMB lease breaks before granting NLM locks
//   - Build NLM4_DENIED responses with SMB holder info
//   - Query SMB lease state for conflict detection
//
// Cross-Protocol Behavior:
// When an NFS client requests a lock that conflicts with an SMB lease, the
// NLM handler must wait for the SMB lease break to complete before proceeding.
// This ensures data consistency when both NFS and SMB clients access the same file.
package handlers

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nlm/types"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Default polling interval for lease break wait
const leaseBreakPollInterval = 100 * time.Millisecond

// buildDeniedResponseFromSMBLease creates an NLM LOCK response for denial due to SMB lease.
//
// When an NLM LOCK request conflicts with an SMB lease, we return NLM4_DENIED
// with holder information translated from the SMB lease. This allows NFS clients
// to see that an SMB client holds a conflicting lease.
//
// Parameters:
//   - cookie: The cookie from the original LOCK request (echoed back)
//   - lease: The conflicting SMB lease (must have Lease != nil)
//
// Returns:
//   - *LockResponse: Response with NLM4_DENIED status and holder info
func buildDeniedResponseFromSMBLease(cookie []byte, lease *lock.EnhancedLock) *LockResponse {
	// Translate SMB lease to NLM holder format
	holderInfo := lock.TranslateToNLMHolder(lease)

	// Build the response
	// Note: LockResponse currently only has Cookie and Status
	// The holder info would be in an extended response type (NLM4_DENIEDargs)
	// For now, we just return NLM4Denied - holder info is logged for debugging
	logger.Info("NLM LOCK denied by SMB lease",
		"caller_name", holderInfo.CallerName,
		"exclusive", holderInfo.Exclusive,
		"lease_key", holderInfo.OH)

	return &LockResponse{
		Cookie: cookie,
		Status: types.NLM4Denied,
	}
}

// waitForLeaseBreak polls until an SMB lease break completes or timeout.
//
// When an NLM operation conflicts with an SMB Write lease, we must wait for
// the SMB client to acknowledge the lease break and flush cached data.
// This function polls at a fixed interval until:
//   - The lease break completes (no more write leases on file)
//   - The context is cancelled
//   - The timeout expires (proceed anyway per Phase 4 decision)
//
// Parameters:
//   - ctx: Context for cancellation
//   - checker: The OplockChecker for querying lease state
//   - handle: File handle to check leases on
//   - timeout: Maximum time to wait for lease break
//
// Returns:
//   - nil when lease break completes or timeout (caller should proceed)
//   - context.Canceled/DeadlineExceeded if context cancelled
func waitForLeaseBreak(ctx context.Context, checker metadata.OplockChecker, handle lock.FileHandle, timeout time.Duration) error {
	if checker == nil {
		return nil
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(leaseBreakPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
			// Check if lease break is still pending
			err := checker.CheckAndBreakForWrite(ctx, handle)
			if err == nil {
				// No more write leases - break complete
				return nil
			}

			// Check if we've exceeded the timeout
			if time.Now().After(deadline) {
				// Timeout - proceed anyway (per Phase 4 decision)
				logger.Info("NLM lease break wait timeout - proceeding",
					"handle", string(handle),
					"timeout", timeout)
				return nil
			}

			// ErrLeaseBreakPending means still waiting - continue polling
			// Any other error - log and continue
			if err.Error() != "lease break pending, operation must wait" {
				logger.Debug("NLM lease break check error - continuing",
					"handle", string(handle),
					"error", err)
			}
		}
	}
}

// getLeaseBreakTimeout returns the configured lease break timeout.
//
// The timeout is read from the config if available, otherwise defaults to 35s.
// Tests can override via DITTOFS_LOCK_LEASE_BREAK_TIMEOUT environment variable.
//
// Parameters:
//   - cfg: The config (may be nil, in which case default is returned)
//
// Returns:
//   - time.Duration: The configured timeout (default 35s)
func getLeaseBreakTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.Lock.LeaseBreakTimeout > 0 {
		return cfg.Lock.LeaseBreakTimeout
	}
	return 35 * time.Second
}

// checkForSMBLeaseConflicts checks if there are SMB leases that would conflict
// with an NLM lock request, and initiates breaks if needed.
//
// This is called before attempting to acquire an NLM lock. If an SMB client
// holds a Write lease, we need to:
//  1. Initiate a lease break to flush cached writes
//  2. Wait for the break to complete
//  3. Then proceed with the NLM lock
//
// Parameters:
//   - ctx: Context for cancellation
//   - checker: The OplockChecker (from MetadataService)
//   - handle: File handle to check
//   - cfg: Config for timeout settings
//
// Returns:
//   - nil if no conflicts or conflicts resolved
//   - error if context cancelled during wait
func checkForSMBLeaseConflicts(ctx context.Context, checker metadata.OplockChecker, handle lock.FileHandle, cfg *config.Config) error {
	if checker == nil {
		return nil
	}

	// Check for write leases and initiate break if needed
	err := checker.CheckAndBreakForWrite(ctx, handle)
	if err == nil {
		return nil
	}

	// Check if it's a pending break (ErrLeaseBreakPending)
	if err.Error() == "lease break pending, operation must wait" {
		// Wait for the break to complete
		timeout := getLeaseBreakTimeout(cfg)
		return waitForLeaseBreak(ctx, checker, handle, timeout)
	}

	// Other error - log and proceed
	logger.Debug("NLM SMB lease check error",
		"handle", string(handle),
		"error", err)
	return nil
}

// buildDeniedResponseFromByteRangeLock creates an NLM LOCK response for denial due to byte-range lock.
//
// Parameters:
//   - cookie: The cookie from the original LOCK request (echoed back)
//   - conflict: The conflicting byte-range lock
//
// Returns:
//   - *LockResponse: Response with NLM4_DENIED status
func buildDeniedResponseFromByteRangeLock(cookie []byte, conflict *lock.EnhancedLock) *LockResponse {
	// Translate to NLM holder format for logging
	holderInfo := lock.TranslateByteRangeLockToNLMHolder(conflict)

	logger.Debug("NLM LOCK denied by byte-range lock",
		"caller_name", holderInfo.CallerName,
		"offset", holderInfo.Offset,
		"length", holderInfo.Length,
		"exclusive", holderInfo.Exclusive)

	return &LockResponse{
		Cookie: cookie,
		Status: types.NLM4Denied,
	}
}
