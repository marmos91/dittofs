package openstate

import "github.com/marmos91/dittofs/pkg/metadata/entity"

// Sink is how open state reaches clients and the service: every other call
// goes into the table, and these come out of it. The service implements it.
// Each method is called after the step that raised it has released every
// table lock, by the goroutine that ran it, and only queues and returns: it
// must not call back into the table before returning, as that goroutine may
// hold the file's admission lock, which a call serving waiters takes again.
type Sink interface {
	// ClientEnded: this node ended a client; the service ends it at each
	// shard's primary.
	ClientEnded(c entity.ClientID, shards []ShardID, why RevokeReason)
	// Closed: open state closed opens of f itself, as an expiry does, and
	// left f an orphan or a pending unlink; the service runs r as it runs a
	// Close's. One call per file carries everything the release left there.
	Closed(f entity.FileID, r CloseResult)
	// LockWaitOver: a callback waiter's wait ended, granted or not; the
	// service forwards it to w.Owner.Client and reports the client's answer to a grant
	// through WaitAnswered.
	LockWaitOver(w LockWaiter, granted bool)
	// RecallGrant: recall g, held by c, to kind to; the service sends it in
	// the holder's protocol, and the client answers through AckRecall. The
	// recall's deadline starts when this call returns.
	RecallGrant(c entity.ClientID, g CachingGrant, to GrantKind)
	// RecallAny: c reached its grant budget here; the service asks it, where
	// its protocol can be asked, to return grants until it holds keep.
	RecallAny(c entity.ClientID, keep int)
	// Revoked: state ended without the client asking; the service tells the
	// client and answers what it holds pending on it.
	Revoked(r Revocation)
}

// Order takes a file's admission lock and returns its unlock. The table takes
// it only where it serves a file's waiters for a step the service held no
// admission lock for, since serving may grant a callback waiter or revoke a
// grant past its recall deadline: on the timer, for a lease expiry, a lapsed
// poll waiter or a recall past its deadline; after AckRecall, FreeState,
// Revoke and Expire return from their own steps; and after Connect and
// ReleaseLockOwner, which name no file. Their callers must hold no admission
// lock.
type Order func(f entity.FileID) (unlock func())

// RevokeReason is why a client's state ended without the client asking.
type RevokeReason uint8

const (
	RevokedLeaseExpired RevokeReason = iota // its lease expired, or a reboot replaced it
	RevokedByAdmin                          // an administrator revoked the client
	RevokedGrant                            // one caching grant whose recall missed its deadline
)

// Revocation names state ended without the client asking: Grant for
// RevokedGrant; every piece of the client's state for RevokedByAdmin.
type Revocation struct {
	Client entity.ClientID
	Grant  GrantID
	Reason RevokeReason
}

// outbox is what a call's steps raised, handed to the Sink once the call holds
// no table lock.
type outbox struct {
	closed map[entity.FileID]CloseResult // one per file: only a file's last close leaves anything
	over   []waitOver
	timer  bool            // raised without the file's admission lock, on the timer or by a call naming no file: waiters are served later, under it
	serve  []entity.FileID // the files whose waiters are still to be served

	revoked []Revocation
	recalls []recallOut
	full    []entity.ClientID // clients that reached their grant budget
}

// recallOut is one RecallGrant, and the recall it hands over: gen names it
// among the grant's recalls, so its deadline is armed only if it is still the
// one in flight.
type recallOut struct {
	g   CachingGrant
	gen uint32
}

// waitOver is one LockWaitOver.
type waitOver struct {
	w       LockWaiter
	granted bool
}

// close records r, what a close the table made itself left f, if it left
// anything.
func (o *outbox) close(f entity.FileID, r CloseResult) {
	if !r.Orphan && r.Unlink == nil {
		return
	}
	if o.closed == nil {
		o.closed = map[entity.FileID]CloseResult{}
	}
	o.closed[f] = r
}

// raise hands o to sink. Each recall's deadline is armed once its
// RecallGrant returns, in a step of its own.
func (o *outbox) raise(t *Table, sink Sink) {
	for f, r := range o.closed {
		sink.Closed(f, r)
	}
	for _, w := range o.over {
		sink.LockWaitOver(w.w, w.granted)
	}
	for _, r := range o.revoked {
		sink.Revoked(r)
	}
	for _, c := range o.full {
		sink.RecallAny(c, keepGrants)
	}
	for _, r := range o.recalls {
		sink.RecallGrant(r.g.Client, r.g, r.g.Recall.To)
		t.idx.Lock()
		t.unsent--
		t.idx.Unlock()
		t.arm(r)
	}
}
