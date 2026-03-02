package lease

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// LeaseBreakNotifier is called when a lease break needs to be sent to a client.
// The implementation should send an SMB2 LEASE_BREAK_NOTIFICATION to the session.
type LeaseBreakNotifier interface {
	// SendLeaseBreak sends a lease break notification to the client.
	// sessionID identifies the session, leaseKey is the 128-bit lease identifier.
	// currentState is the client's current state, newState is the target state.
	// epoch is the SMB3 epoch counter (NewEpoch in V2 break notifications).
	SendLeaseBreak(sessionID uint64, leaseKey [16]byte, currentState, newState uint32, epoch uint16) error
}

// SMBBreakHandler implements lock.BreakCallbacks for SMB lease break dispatch.
//
// When the LockManager dispatches an oplock/lease break, this handler:
//  1. Looks up the SMB sessionID for the lease via LeaseManager
//  2. Sends a LEASE_BREAK_NOTIFICATION to the client via LeaseBreakNotifier
//
// This replaces the break notification logic that was in OplockManager.
type SMBBreakHandler struct {
	leaseManager *LeaseManager
	notifier     LeaseBreakNotifier
}

// NewSMBBreakHandler creates a new SMBBreakHandler.
func NewSMBBreakHandler(leaseManager *LeaseManager, notifier LeaseBreakNotifier) *SMBBreakHandler {
	return &SMBBreakHandler{
		leaseManager: leaseManager,
		notifier:     notifier,
	}
}

// OnOpLockBreak is called when an oplock/lease must be broken.
// It dispatches the break notification to the SMB client via the transport notifier.
//
// Per CONTEXT.md: "if the lease was created with V2 context, send V2
// LEASE_BREAK_Notification (includes ParentLeaseKey + epoch)". The NewEpoch
// field is set to Epoch+1 for 3.x dialect leases.
func (h *SMBBreakHandler) OnOpLockBreak(handleKey string, ul *lock.UnifiedLock, breakToState uint32) {
	if ul == nil || ul.Lease == nil {
		return
	}

	// Look up session for this lease
	sessionID, found := h.leaseManager.GetSessionForLease(ul.Lease.LeaseKey)
	if !found {
		logger.Debug("SMBBreakHandler: no session for lease, skipping break notification",
			"leaseKey", fmt.Sprintf("%x", ul.Lease.LeaseKey),
			"handleKey", handleKey)
		return
	}

	// Resolve notifier: prefer direct notifier, fall back to lease manager's
	notifier := h.notifier
	if notifier == nil {
		notifier = h.leaseManager.GetNotifier()
		if notifier == nil {
			logger.Warn("SMBBreakHandler: no notifier available for lease break",
				"leaseKey", fmt.Sprintf("%x", ul.Lease.LeaseKey))
			return
		}
	}

	// Use the current lease epoch for V2 break notifications. The LockManager
	// already advanced the epoch when initiating the break.
	newEpoch := ul.Lease.Epoch

	logger.Debug("SMBBreakHandler: dispatching lease break notification",
		"leaseKey", fmt.Sprintf("%x", ul.Lease.LeaseKey),
		"sessionID", sessionID,
		"currentState", lock.LeaseStateToString(ul.Lease.LeaseState),
		"breakToState", lock.LeaseStateToString(breakToState),
		"epoch", newEpoch)

	// Send break notification asynchronously to avoid blocking the LockManager
	go func() {
		if err := notifier.SendLeaseBreak(sessionID, ul.Lease.LeaseKey, ul.Lease.LeaseState, breakToState, newEpoch); err != nil {
			logger.Warn("SMBBreakHandler: failed to send lease break notification",
				"leaseKey", fmt.Sprintf("%x", ul.Lease.LeaseKey),
				"sessionID", sessionID,
				"error", err)
		}
	}()
}

// OnByteRangeRevoke is called when a byte-range lock must be revoked.
// For SMB, this is a no-op since SMB byte-range locks don't have async revocation.
func (h *SMBBreakHandler) OnByteRangeRevoke(_ string, _ *lock.UnifiedLock, _ string) {
	// No-op: SMB byte-range lock revocation is handled synchronously
}

// OnAccessConflict is called when an SMB access mode conflict is detected.
// For SMB, this is a no-op since access conflicts are returned as status codes.
func (h *SMBBreakHandler) OnAccessConflict(_ string, _ *lock.UnifiedLock, _ lock.AccessMode) {
	// No-op: access conflicts are returned as STATUS_SHARING_VIOLATION
}

// SMBOplockBreaker implements the adapter.OplockBreaker interface using
// the shared LockManager, replacing the old OplockManager-based implementation.
//
// This enables NFS handlers to trigger lease breaks on SMB clients without
// importing the SMB handler package.
//
// The resolver provides the per-share LockManager, and the handleKeyResolver
// extracts the share name from a file handle to route to the correct LockManager.
type SMBOplockBreaker struct {
	resolver LockManagerResolver
}

// NewSMBOplockBreaker creates a new cross-protocol oplock breaker.
func NewSMBOplockBreaker(resolver LockManagerResolver) *SMBOplockBreaker {
	return &SMBOplockBreaker{resolver: resolver}
}

// CheckAndBreakForWrite triggers lease break for write-conflicting oplocks.
func (b *SMBOplockBreaker) CheckAndBreakForWrite(_ context.Context, fileHandle lock.FileHandle) error {
	handleKey := string(fileHandle)
	lockMgr := b.resolveLockManagerForHandle(handleKey)
	if lockMgr == nil {
		return nil
	}
	return lockMgr.CheckAndBreakOpLocksForWrite(handleKey, nil)
}

// CheckAndBreakForRead triggers lease break for read-conflicting oplocks.
func (b *SMBOplockBreaker) CheckAndBreakForRead(_ context.Context, fileHandle lock.FileHandle) error {
	handleKey := string(fileHandle)
	lockMgr := b.resolveLockManagerForHandle(handleKey)
	if lockMgr == nil {
		return nil
	}
	return lockMgr.CheckAndBreakOpLocksForRead(handleKey, nil)
}

// CheckAndBreakForDelete triggers lease break for Handle leases before deletion.
func (b *SMBOplockBreaker) CheckAndBreakForDelete(_ context.Context, fileHandle lock.FileHandle) error {
	handleKey := string(fileHandle)
	lockMgr := b.resolveLockManagerForHandle(handleKey)
	if lockMgr == nil {
		return nil
	}
	return lockMgr.CheckAndBreakOpLocksForDelete(handleKey, nil)
}

// resolveLockManagerForHandle attempts to resolve a LockManager from a file handle.
// File handles encode the share name, but we cannot import the metadata package here
// to decode them. Instead, we try all known shares via the resolver.
func (b *SMBOplockBreaker) resolveLockManagerForHandle(handleKey string) lock.LockManager {
	if b.resolver == nil {
		return nil
	}
	// The resolver knows all shares. For cross-protocol breaks, we need the
	// LockManager that owns the handle. Since file handles encode the share
	// name as a prefix, the resolver implementation can extract and route.
	// For now, use the AllSharesLockManager if the resolver supports it.
	if allShares, ok := b.resolver.(AllSharesResolver); ok {
		return allShares.GetLockManagerForHandle(handleKey)
	}
	return nil
}

// AllSharesResolver is an optional interface that resolvers can implement
// to provide handle-based LockManager resolution.
type AllSharesResolver interface {
	LockManagerResolver
	// GetLockManagerForHandle returns the LockManager for a file handle.
	GetLockManagerForHandle(handleKey string) lock.LockManager
}
