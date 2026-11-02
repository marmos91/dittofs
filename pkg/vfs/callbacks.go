package vfs

import "context"

// Callbacks is implemented by each adapter and registered per client at
// Connect, the latest Connect's replacing any earlier: the service calls up
// through it and never imports an adapter.
// A callback must not block past its context's deadline, and one that fails
// or times out is taken as the client not answering: a recall unanswered by
// its deadline revokes the grant.
type Callbacks interface {
	// RecallGrant is an NFS CB_RECALL on the client's back channel, s zero
	// and openers nil, or an SMB lease or oplock break on session s, one
	// holding an open g covers, which the service picked: the adapter
	// delivers it there and nowhere else. With s zero and openers given,
	// every open of g is disconnected: the adapter sends it on one of its own
	// sessions of the client signed in now as one of openers, or on none.
	// sent is whether it sent.
	RecallGrant(ctx context.Context, s SessionRef, openers []Principal, g CachingGrant, to GrantKind) (sent bool, err error)
	// Notify delivers the changes watch w matched.
	Notify(ctx context.Context, w WatchID, changes []Change) error
	// Revoked is state ended without the client asking: the adapter answers
	// what it holds pending on it.
	Revoked(ctx context.Context, what Revocation)
	// LockWaitOver ends a lock wait, granted or not. An error is no answer by
	// the deadline, and a grant is then sent again; refused is the client
	// refusing the grant, and the lock is released.
	LockWaitOver(ctx context.Context, w LockWaiter, granted bool) (refused bool, err error)
	// RecallAny asks the client to return grants until it holds keep; a
	// protocol that cannot be asked answers nil.
	RecallAny(ctx context.Context, keep int) error
}
