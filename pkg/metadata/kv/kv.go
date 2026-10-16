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
	// Limits is what one transaction may hold. The limits are the backend's,
	// never settings, and the entity layer derives its batch sizes from them.
	Limits() TxnLimits
	// Close waits for the calls in flight, each of which completes as it
	// would have, then releases the store. Every call after it fails with
	// ErrClosed, and a second Close does nothing.
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
	// Before returns the last record under prefix whose key sorts below key;
	// ok is false when there is none. It finds the record covering an
	// offset — the one starting at or before it — in one seek, whatever the
	// distance. Like Scan it is never tracked, and inside Update it sees the
	// transaction's own writes, deletions included.
	Before(prefix, key []byte) (kv KeyValue, ok bool, err error)
	// Versions returns each key's change sequence, in one batched read: the
	// version the store assigned the commit that last wrote the key, ordering
	// the writes of one key as their commits were ordered. A key absent from
	// the snapshot has 0. Inside Update a key the transaction set or deleted
	// has no version until the commit, so the call fails with
	// ErrUncommittedVersion rather than guess one; any other key reports what
	// the snapshot holds. It is not tracked.
	Versions(keys [][]byte) ([]uint64, error)
}

// Txn is a read-write transaction.
type Txn interface {
	Reader
	// Set and Delete conflict with any concurrent write of key, a blind one
	// included. A transaction that goes past Limits gets ErrTooLarge, from the
	// call that took it there: a Set or Delete, or a Get, Guard or GuardRange,
	// whose conflict ranges count toward TxnBytes.
	Set(key, value []byte) error
	Delete(key []byte) error
	// Guard is a tracked read that returns no value: the commit aborts if
	// another transaction committed a write of key after the snapshot, key
	// absent or not. A guard binds only its own transaction: a write of key
	// that commits after the guarding transaction did is not refused, since it
	// is then simply ordered after it, so a writer whose decision rests on
	// what the guarding transaction wrote must track a key it writes. Two
	// guards never conflict, point or range, whatever their commit order. A
	// guard constrains only a transaction that writes: a backend may skip the
	// check of one that writes nothing, so no caller may rely on a guard there.
	Guard(key []byte) error
	// GuardRange is Guard for every key under prefix, present or not: the
	// commit aborts if another transaction committed a write of any key under
	// prefix after the snapshot. It is how a check over a range ("the
	// directory is empty") is made safe against a concurrent insert. Range
	// guards are shared like point guards.
	GuardRange(prefix []byte) error
	// Now is store time for this transaction: strictly increasing across
	// transactions that commit in order, within a stated bound of real time,
	// and fixed for the transaction's life. Reading it writes no key.
	Now() time.Time
}

// KeyValue is one record a Scan yields. Key and Value are valid only until
// the iteration moves on.
type KeyValue struct {
	Key, Value []byte
}

// TxnLimits bounds one transaction.
type TxnLimits struct {
	Key, Value int // the largest key and the largest value, in bytes
	// TxnBytes bounds one transaction counted with its conflict ranges: the
	// key and value bytes of every Set, the key bytes of every Delete, Get and
	// Guard, and the prefix bytes of every GuardRange.
	TxnBytes int
	// TxnEntries bounds the keys one transaction sets or deletes; 0 if
	// unbounded.
	TxnEntries int
	// Age bounds a transaction from its snapshot to its commit: an older one
	// aborts and is retried like a conflict.
	Age time.Duration
}

var (
	ErrNotFound = errors.New("kv: key not found")
	// ErrConflict is a serialisation conflict. Update retries it; it reaches a
	// caller only joined with ctx's error, once ctx is done.
	ErrConflict = errors.New("kv: transaction conflict")
	ErrTooLarge = errors.New("kv: transaction exceeds limits")
	// ErrUncommittedVersion is Versions of a key the transaction wrote.
	ErrUncommittedVersion = errors.New("kv: version of a key written in this transaction")
	// ErrClosed is a call on a store after its Close.
	ErrClosed = errors.New("kv: store closed")
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
