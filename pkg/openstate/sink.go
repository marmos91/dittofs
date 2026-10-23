package openstate

import "github.com/marmos91/dittofs/pkg/metadata/entity"

// Sink is how open state reaches clients and the service: every other call
// goes into the table, and these come out of it. The service implements it.
// Each method is called after the step that raised it has released every
// table lock, by the goroutine that ran it, and only queues and returns.
type Sink interface {
	// ClientEnded: this node ended a client; the service ends it at each
	// shard's primary.
	ClientEnded(c entity.ClientID, shards []ShardID, why RevokeReason)
	// Closed: open state closed opens of f itself, as an expiry does, and
	// left f an orphan or a pending unlink; the service runs r as it runs a
	// Close's. One call per file carries everything the release left there.
	Closed(f entity.FileID, r CloseResult)
	// LockWaitOver: a callback waiter's wait ended, granted or not; the
	// service forwards it to c and reports the client's answer to a grant
	// through WaitAnswered.
	LockWaitOver(c entity.ClientID, w LockWaiter, granted bool)
}

// Order takes a file's admission lock and returns its unlock. The table takes
// it only for the one timer step that adds state: granting a callback waiter
// on a range the timer freed.
type Order func(f entity.FileID) (unlock func())

// RevokeReason is why a client's state ended without the client asking.
type RevokeReason uint8

const (
	RevokedLeaseExpired RevokeReason = iota // its lease expired, or a reboot replaced it
)

// outbox is what a call's steps raised, handed to the Sink once the call holds
// no table lock.
type outbox struct {
	closed map[entity.FileID]CloseResult // one per file: only a file's last close leaves anything
	over   []waitOver
	timer  bool            // raised on the timer: waiters are served later, under the file's admission lock
	serve  []entity.FileID // the files whose waiters are still to be served
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

func (o *outbox) raise(sink Sink) {
	for f, r := range o.closed {
		sink.Closed(f, r)
	}
	for _, w := range o.over {
		sink.LockWaitOver(w.w.Owner.Client, w.w, w.granted)
	}
}
