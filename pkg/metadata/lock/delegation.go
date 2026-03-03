// Package lock provides delegation management for cross-protocol caching.
//
// Delegations are the protocol-neutral equivalent of NFS delegations and
// SMB leases, representing caching permissions granted to a client. Unlike
// leases (which are SMB-specific with LeaseKey and R/W/H flags), delegations
// are a simpler read/write model that can be mapped to either protocol.
//
// This file contains the Delegation struct, type enum, coexistence rules
// with leases, and helper functions.
//
// Reference: RFC 8881 Section 10 (NFS Delegations)
package lock

import (
	"time"

	"github.com/google/uuid"
)

// DelegationType represents the type of delegation (read or write).
type DelegationType int

const (
	// DelegTypeRead is a read delegation - client may cache reads.
	// Multiple read delegations can coexist on the same file.
	DelegTypeRead DelegationType = iota

	// DelegTypeWrite is a write delegation - client may cache writes.
	// Only one write delegation can exist per file. Write delegations
	// conflict with both read and write leases.
	DelegTypeWrite
)

// String returns a human-readable name for the delegation type.
func (dt DelegationType) String() string {
	switch dt {
	case DelegTypeRead:
		return "read"
	case DelegTypeWrite:
		return "write"
	default:
		return "unknown"
	}
}

// Delegation holds protocol-neutral delegation state.
//
// A delegation grants a client permission to cache file data locally,
// reducing network round-trips. The server can recall delegations when
// another client needs access.
//
// No NFS-specific fields (no Stateid4, no *time.Timer) are stored here.
// Protocol adapters map between this struct and their protocol-specific types.
//
// Lifecycle:
//  1. Client requests delegation via protocol handler
//  2. LockManager grants delegation (stored as UnifiedLock with Delegation field)
//  3. Conflicting operation triggers recall (Breaking=true, BreakStarted set)
//  4. Client returns delegation or timeout expires
//  5. Delegation removed from lock manager
type Delegation struct {
	// DelegationID is a unique identifier for this delegation (UUID).
	DelegationID string

	// DelegType is the type of delegation (read or write).
	DelegType DelegationType

	// IsDirectory indicates this delegation is on a directory.
	IsDirectory bool

	// ClientID identifies the client holding the delegation.
	ClientID string

	// ShareName is the share this delegation belongs to.
	ShareName string

	// Breaking indicates a delegation recall is in progress.
	// When true, the client has been notified to return the delegation.
	Breaking bool

	// BreakStarted records when the recall was initiated.
	// Used to enforce recall timeout (force revoke if client does not return).
	BreakStarted time.Time

	// Recalled indicates the delegation recall notification was sent.
	Recalled bool

	// Revoked indicates the delegation was force-revoked (timeout expired).
	Revoked bool

	// NotificationMask is a bitmask of directory change notification types
	// this delegation is interested in. Used for directory delegations.
	NotificationMask uint32
}

// NewDelegation creates a new Delegation with a generated UUID.
func NewDelegation(delegType DelegationType, clientID, shareName string, isDirectory bool) *Delegation {
	return &Delegation{
		DelegationID: uuid.New().String(),
		DelegType:    delegType,
		ClientID:     clientID,
		ShareName:    shareName,
		IsDirectory:  isDirectory,
	}
}

// Clone returns a deep copy of the Delegation.
// Returns nil if the receiver is nil.
func (d *Delegation) Clone() *Delegation {
	if d == nil {
		return nil
	}
	return &Delegation{
		DelegationID:     d.DelegationID,
		DelegType:        d.DelegType,
		IsDirectory:      d.IsDirectory,
		ClientID:         d.ClientID,
		ShareName:        d.ShareName,
		Breaking:         d.Breaking,
		BreakStarted:     d.BreakStarted,
		Recalled:         d.Recalled,
		Revoked:          d.Revoked,
		NotificationMask: d.NotificationMask,
	}
}

// DelegationConflictsWithLease checks if a delegation conflicts with an SMB lease.
//
// Coexistence rules:
//   - Read delegation + Read-only lease = OK (both are read-only caching)
//   - Write delegation + any lease = CONFLICT (write delegation is exclusive)
//   - Any delegation + Write lease = CONFLICT (write lease requires exclusive access)
//   - Read delegation + Read+Handle lease = OK (no write involved)
//
// Returns false if either input is nil.
func DelegationConflictsWithLease(deleg *Delegation, lease *OpLock) bool {
	if deleg == nil || lease == nil {
		return false
	}

	// Write delegation conflicts with any active lease (not LeaseStateNone)
	if deleg.DelegType == DelegTypeWrite {
		return lease.LeaseState != LeaseStateNone
	}

	// Any delegation conflicts with write lease
	if lease.HasWrite() {
		return true
	}

	// Read delegation + non-write lease = OK
	return false
}
