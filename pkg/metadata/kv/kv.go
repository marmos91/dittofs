// Package kv is the transactional key-value interface every metadata backend
// implements, and the one the entity layer is written against. Its semantics
// are fixed by the KV conformance suite in kvtest; a backend that passes it is
// interchangeable with any other.
package kv

import (
	"context"
	"errors"
	"iter"
	"math/rand/v2"
	"time"
)

// KV is a backend. Update and View each run fn in one transaction against one
// consistent snapshot.
type KV interface {
	// Update runs fn in one read-write transaction and retries it on a
	// serialisation conflict until ctx is done, so fn may run more than once
	// and must have no effect outside the transaction. Update returns success
	// only once the transaction's writes are durable. An error does not mean
	// nothing committed: callers treat it as an unknown outcome.
	Update(ctx context.Context, fn func(Txn) error) error
	// View runs fn in one read-only transaction.
	View(ctx context.Context, fn func(Reader) error) error
	// Limits is the largest transaction the backend commits.
	Limits() TxnLimits
	Close() error
}

// Reader reads one snapshot.
type Reader interface {
	// Get returns the value of key, or ErrNotFound. Inside Update the read is
	// tracked: the commit aborts if another transaction committed a write of
	// key after the snapshot.
	Get(key []byte) ([]byte, error)
	// Scan yields the keys under prefix that sort after after (all of them
	// when after is nil), in key order. A scan is never tracked, so it
	// detects no phantom: an invariant over a range uses GuardRange or gates
	// on a point record.
	// Inside Update it sees the transaction's own writes.
	Scan(prefix, after []byte) iter.Seq2[KeyValue, error]
}

// Txn is a read-write transaction.
type Txn interface {
	Reader
	// Set and Delete conflict with any concurrent write of key, a blind one
	// included. A transaction that writes past Limits gets ErrTooLarge.
	Set(key, value []byte) error
	Delete(key []byte) error
	// Guard is a tracked read that returns no value: the commit aborts if
	// another transaction committed a write of key after the snapshot, key
	// absent or not. Guards are shared: two transactions that only guard one
	// key never conflict. A guard binds only its own transaction: a write of
	// key that commits after the guarding transaction did is not refused,
	// since it is then simply ordered after it. A writer whose decision rests
	// on a range must therefore guard the range itself (GuardRange).
	Guard(key []byte) error
	// GuardRange is Guard for every key under prefix, present or not: the
	// commit aborts if another transaction committed a write of any key under
	// prefix after the snapshot. It is how a check over a range ("the
	// directory is empty") is made safe against a concurrent insert. Range
	// guards are shared like point guards.
	GuardRange(prefix []byte) error
	// Now is store time for this transaction: monotonic across transactions
	// that commit in order, within a stated bound of real time, and fixed for
	// the transaction's life. Reading it writes no key.
	Now() time.Time
}

// KeyValue is one record a Scan yields. Key and Value are valid only until
// the iteration moves on.
type KeyValue struct {
	Key, Value []byte
	// Seq is the record's change sequence: the commit version the store keeps
	// with the record, ordering records as their commits were ordered when
	// compared with bytes.Compare. It is nil for the transaction's own writes,
	// which have not committed.
	Seq []byte
}

// TxnLimits bounds one transaction. Entries and Bytes bound the whole
// transaction, Bytes counting key and value bytes of every Set and the key
// bytes of every Delete; Key and Value bound one entry.
type TxnLimits struct {
	Entries, Bytes int
	Key, Value     int
}

var (
	ErrNotFound = errors.New("kv: key not found")
	// ErrConflict is a serialisation conflict. Update retries it; it reaches a
	// caller only joined with ctx's error, once ctx is done.
	ErrConflict = errors.New("kv: transaction conflict")
	ErrTooLarge = errors.New("kv: transaction exceeds limits")
)

// Retry runs attempt until it returns anything but ErrConflict or ctx is done,
// sleeping a randomised, growing backoff between attempts so the losers of one
// conflict do not collide again on the next. Backends build Update on it.
func Retry(ctx context.Context, attempt func() error) error {
	backoff := 50 * time.Microsecond
	for {
		err := attempt()
		if !errors.Is(err, ErrConflict) {
			return err
		}
		t := time.NewTimer(rand.N(backoff) + 1)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
		backoff = min(2*backoff, 10*time.Millisecond)
	}
}
