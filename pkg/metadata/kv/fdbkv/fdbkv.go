//go:build fdb

// Package fdbkv is the replicated kv backend, on FoundationDB.
package fdbkv

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/internal/backend"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// apiVersion is the client API this package is written against, 7.3's.
const apiVersion = 730

// Options configures a Store.
type Options struct {
	// ClusterFile names the cluster; empty is the client's default.
	ClusterFile string
	// Prefix is put before every key, so stores can share a cluster.
	Prefix []byte
	// NodeID names this node's clock record, N‖node‖clk.
	NodeID [keys.IDLen]byte
	// Clock is the node's clock; nil is time.Now.
	Clock func() time.Time
}

// Store is a kv.KV on FoundationDB. A commit is durable once FoundationDB
// acknowledges it, so Update needs no sync of its own.
//
// Every value is stored behind a 10-byte versionstamp the commit writes, the
// change sequence Versions reports. FoundationDB refuses to read back a value
// stamped in the same transaction, so a transaction's writes stay in an
// overlay its reads merge, and reach FoundationDB only at commit.
type Store struct {
	db     fdb.Database
	prefix []byte
	end    []byte // the first key past prefix's range
	clk    []byte // this node's clock-ceiling key, unprefixed
	time   *backend.Clock
	limits kv.TxnLimits

	stop chan struct{}
	done chan struct{}

	// gate is held shared by every Update and View attempt and exclusively
	// by Close, which so waits for the calls in flight. closed is set under
	// it.
	gate   sync.RWMutex
	closed bool
}

var apiOnce = sync.OnceValue(func() error { return fdb.APIVersion(apiVersion) })

// Open connects to the cluster and raises this node's clock ceiling before
// returning, so no Now precedes a durable ceiling.
func Open(opts Options) (*Store, error) {
	if err := apiOnce(); err != nil {
		return nil, fmt.Errorf("fdbkv: client API %d: %w", apiVersion, err)
	}
	db, err := fdb.OpenDatabase(opts.ClusterFile)
	if err != nil {
		return nil, fmt.Errorf("fdbkv: %w", err)
	}
	s := &Store{
		db:     db,
		prefix: bytes.Clone(opts.Prefix),
		clk:    append(keys.ID([]byte{keys.KindN}, opts.NodeID[:]), keys.NClk),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if len(s.prefix) > 0 {
		if s.end, err = fdb.Strinc(s.prefix); err != nil {
			return nil, fmt.Errorf("fdbkv: prefix %x: %w", s.prefix, err)
		}
	} else {
		s.end = []byte{0xff} // the system keys start there
	}
	s.limits = kv.TxnLimits{
		Key:      maxKey - len(s.prefix) - 1,
		Value:    maxValue - stampSize - 4,
		TxnBytes: txnBytes,
		Age:      maxAge,
	}

	// A start resumes from the stored ceiling, so a clock stepped back across
	// a restart cannot carry Now backward.
	stored, err := s.readCeiling()
	if err != nil {
		return nil, err
	}
	s.time = backend.NewClock(opts.Clock, stored)
	if err := s.time.Raise(s.writeCeiling); err != nil {
		return nil, err
	}
	go func() {
		defer close(s.done)
		s.time.Run(s.stop, s.writeCeiling)
	}()
	return s, nil
}

// FoundationDB's own limits: a key of 10,000 bytes, a value of 100,000, and a
// transaction of 10,000,000 counted with its conflict ranges, which must be
// read within five seconds of its read version.
const (
	maxKey   = 10_000
	maxValue = 100_000
	maxTxn   = 10_000_000
	maxAge   = 5 * time.Second

	stampSize = 10
)

// txnBytes is TxnBytes, what a transaction may hold counted the kv way.
//
// decision: FoundationDB counts a write's key three times (the mutation and
// its read conflict range's two ends) and every key with the store's prefix,
// so TxnBytes declares under a third of its limit: a transaction of the
// store's keys, 17 bytes and more, that fits TxnBytes also fits FoundationDB.
// A transaction of much shorter keys can reach FoundationDB's limit first and
// get ErrTooLarge from the estimate (txn.size) before TxnBytes. Raise it with
// a measurement of what the store's transactions actually cost.
const txnBytes = 3_000_000

func (s *Store) Limits() kv.TxnLimits { return s.limits }

// Close waits for the calls in flight and stops the store. It leaves the
// database handle open.
//
// decision: the client hands every Open of one cluster file the same cached
// handle, and closing it would pull it from under every other store on that
// cluster in the process. A handle is meant to live as long as the process;
// release it here only if Open ever creates one of its own.
func (s *Store) Close() error {
	s.gate.Lock()
	if s.closed {
		s.gate.Unlock()
		return nil
	}
	s.closed = true
	s.gate.Unlock()
	close(s.stop)
	<-s.done
	return nil
}

func (s *Store) enter() error {
	s.gate.RLock()
	if s.closed {
		s.gate.RUnlock()
		return kv.ErrClosed
	}
	return nil
}

func (s *Store) leave() { s.gate.RUnlock() }

func (s *Store) key(k []byte) fdb.Key {
	out := make([]byte, 0, len(s.prefix)+len(k))
	return append(append(out, s.prefix...), k...)
}

func (s *Store) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return kv.Retry(ctx, func() error { return s.attempt(ctx, fn) })
}

func (s *Store) View(ctx context.Context, fn func(kv.Reader) error) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	tr, stop, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer stop()
	return mapErr(fn(&reader{s: s, snap: tr.Snapshot()}))
}

// begin opens a transaction that ctx bounds: its deadline becomes the
// transaction's timeout, and its cancellation cancels it. stop releases the
// watcher and the transaction.
func (s *Store) begin(ctx context.Context) (fdb.Transaction, func(), error) {
	tr, err := s.db.CreateTransaction()
	if err != nil {
		return tr, nil, mapErr(err)
	}
	if d, ok := ctx.Deadline(); ok {
		if err := tr.Options().SetTimeout(max(1, time.Until(d).Milliseconds())); err != nil {
			return tr, nil, err
		}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			tr.Cancel()
		case <-done:
		}
	}()
	return tr, func() { close(done); tr.Cancel() }, nil
}

func (s *Store) attempt(ctx context.Context, fn func(kv.Txn) error) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	tr, stop, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer stop()
	start := time.Now()
	// The snapshot is the attempt's start. FoundationDB takes a read version
	// at the first read otherwise, and a transaction that only guards would
	// take it at its commit, after any write it should conflict with.
	if _, err := tr.GetReadVersion().Get(); err != nil {
		return mapErr(err)
	}
	t := &txn{reader: reader{s: s, snap: tr.Snapshot(), own: backend.Overlay{}}, tr: tr, now: s.time.Now()}
	if err := fn(t); err != nil {
		return mapErr(err)
	}
	if len(t.own) == 0 {
		// Nothing to commit: FoundationDB checks no conflict of a
		// transaction that writes nothing, and its reads were of
		// committed, durable data.
		return nil
	}
	if age := time.Since(start); age > s.limits.Age {
		return fmt.Errorf("%w: transaction open %v, past its age limit of %v", kv.ErrConflict, age, s.limits.Age)
	}
	for k, w := range t.own {
		key := s.key([]byte(k))
		// The read conflict makes two blind writers of key conflict:
		// FoundationDB checks reads against writes only.
		if err := tr.AddReadConflictKey(key); err != nil {
			return mapErr(err)
		}
		if w.Deleted {
			tr.Clear(key)
		} else {
			tr.SetVersionstampedValue(key, stamped(w.Value))
		}
	}
	if err := tr.Commit().Get(); err != nil {
		// decision: an unknown commit result is returned, not retried. The
		// commit may have landed, and running fn again over its own
		// result would answer as if it had not: a create would find its
		// file and report it exists. The error leaves the outcome
		// unknown, as kv.KV says an Update error does.
		if code(err) == codeCommitUnknown {
			return fmt.Errorf("fdbkv: commit outcome unknown: %w", err)
		}
		return mapErr(err)
	}
	return nil
}

// FoundationDB's error codes the store tells apart.
const (
	codeTooOld        = 1007 // transaction_too_old
	codeFutureVersion = 1009 // future_version
	codeNotCommitted  = 1020 // not_committed: a conflict
	codeCommitUnknown = 1021 // commit_unknown_result
	codeCanceled      = 1025 // transaction_cancelled
	codeTimedOut      = 1031 // transaction_timed_out
	codeProcessBehind = 1037 // process_behind
	codeTooLarge      = 2101 // transaction_too_large
	codeKeyTooLarge   = 2102
	codeValueTooLarge = 2103
)

func code(err error) int {
	var e fdb.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// mapErr turns FoundationDB's retryable errors into kv.ErrConflict, so Update
// retries them, and its size errors into kv.ErrTooLarge.
func mapErr(err error) error {
	switch code(err) {
	case 0:
		return err
	case codeNotCommitted, codeTooOld, codeFutureVersion, codeProcessBehind:
		return fmt.Errorf("%w: %w", kv.ErrConflict, err)
	case codeTooLarge, codeKeyTooLarge, codeValueTooLarge:
		return fmt.Errorf("%w: %w", kv.ErrTooLarge, err)
	case codeCanceled, codeTimedOut:
		return fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
	}
	return err
}

// stamped is SetVersionstampedValue's parameter for value: ten bytes the
// commit fills with its versionstamp, the value, and the stamp's offset.
func stamped(value []byte) []byte {
	p := make([]byte, stampSize, stampSize+len(value)+4)
	p = append(p, value...)
	return binary.LittleEndian.AppendUint32(p, 0)
}

// unstamp splits a stored value into its commit version and the value.
func unstamp(v []byte) (uint64, []byte, error) {
	if len(v) < stampSize {
		return 0, nil, fmt.Errorf("fdbkv: stored value of %d bytes holds no versionstamp", len(v))
	}
	return binary.BigEndian.Uint64(v[:8]), v[stampSize:], nil
}

func (s *Store) readCeiling() (time.Time, error) {
	var c time.Time
	err := s.View(context.Background(), func(r kv.Reader) error {
		b, err := r.Get(s.clk)
		if errors.Is(err, kv.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		c, err = backend.DecodeCeiling(b)
		return err
	})
	return c, err
}

func (s *Store) writeCeiling(v []byte) error {
	return s.Update(context.Background(), func(t kv.Txn) error { return t.Set(s.clk, v) })
}

// reader reads one snapshot. Its reads are untracked; txn tracks what the
// contract tracks.
type reader struct {
	s    *Store
	snap fdb.Snapshot
	own  backend.Overlay // the transaction's own writes; nil in a View
}

func (r *reader) Get(key []byte) ([]byte, error) {
	if w, ok := r.own[string(key)]; ok {
		if w.Deleted {
			return nil, kv.ErrNotFound
		}
		return bytes.Clone(w.Value), nil
	}
	return r.get(r.snap, key)
}

func (r *reader) get(rt fdb.ReadTransaction, key []byte) ([]byte, error) {
	v, err := rt.Get(r.s.key(key)).Get()
	if err != nil {
		return nil, mapErr(err)
	}
	if v == nil {
		return nil, kv.ErrNotFound
	}
	_, val, err := unstamp(v)
	return val, err
}

func (r *reader) Scan(prefix, after []byte) iter.Seq2[kv.KeyValue, error] {
	return func(yield func(kv.KeyValue, error) bool) {
		begin := r.s.key(prefix)
		if after != nil && bytes.Compare(after, prefix) >= 0 {
			begin = append(r.s.key(after), 0)
		}
		end, err := r.s.rangeEnd(prefix)
		if err != nil {
			yield(kv.KeyValue{}, err)
			return
		}
		it := r.snap.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
		r.own.Scan(prefix, after, r.s.cursor(it), yield)
	}
}

func (r *reader) Before(prefix, key []byte) (kv.KeyValue, bool, error) {
	end, err := r.s.rangeEnd(prefix)
	if err != nil {
		return kv.KeyValue{}, false, err
	}
	if bytes.Compare(key, prefix) < 0 {
		return kv.KeyValue{}, false, nil
	}
	if k := r.s.key(key); bytes.Compare(k, end) < 0 {
		end = k
	}
	it := r.snap.GetRange(fdb.KeyRange{Begin: r.s.key(prefix), End: end}, fdb.RangeOptions{Mode: fdb.StreamingModeIterator, Reverse: true}).Iterator()
	return r.own.Before(prefix, key, r.s.cursor(it))
}

// rangeEnd is the first stored key past every key under prefix.
func (s *Store) rangeEnd(prefix []byte) (fdb.Key, error) {
	if len(prefix) == 0 {
		return s.end, nil
	}
	e, err := fdb.Strinc(prefix)
	if err != nil {
		// prefix is all 0xff: everything after it in the store's range.
		return s.end, nil
	}
	return s.key(e), nil
}

// Versions reads each key's versionstamp in one batch of concurrent reads,
// untracked, and refuses a key the transaction wrote: its stamp is the
// commit's, which FoundationDB writes only then.
func (r *reader) Versions(ks [][]byte) ([]uint64, error) {
	futures := make([]fdb.FutureByteSlice, len(ks))
	for i, k := range ks {
		if r.own.Has(k) {
			return nil, kv.ErrUncommittedVersion
		}
		futures[i] = r.snap.Get(r.s.key(k))
	}
	vs := make([]uint64, len(ks))
	for i, f := range futures {
		v, err := f.Get()
		if err != nil {
			return nil, mapErr(err)
		}
		if v == nil {
			continue
		}
		if vs[i], _, err = unstamp(v); err != nil {
			return nil, err
		}
	}
	return vs, nil
}

// cursor walks a range read for the overlay's merge, stripping the store's
// prefix from keys and the versionstamp from values.
type cursor struct {
	s     *Store
	it    *fdb.RangeIterator
	cur   fdb.KeyValue
	valid bool
	err   error
}

func (s *Store) cursor(it *fdb.RangeIterator) *cursor {
	c := &cursor{s: s, it: it}
	c.Next()
	return c
}

func (c *cursor) Valid() bool { return c.valid }
func (c *cursor) Err() error  { return c.err }
func (c *cursor) Key() []byte { return c.cur.Key[len(c.s.prefix):] }

func (c *cursor) Value() ([]byte, error) {
	_, v, err := unstamp(c.cur.Value)
	return v, err
}

func (c *cursor) Next() {
	c.valid = false
	if c.err != nil || !c.it.Advance() {
		return
	}
	kv, err := c.it.Get()
	if err != nil {
		c.err = mapErr(err)
		return
	}
	c.cur, c.valid = kv, true
}

// txn is one attempt of an Update.
type txn struct {
	reader
	tr  fdb.Transaction
	now time.Time

	bytes int // TxnBytes, as kv counts it
	size  int // FoundationDB's size of it: mutations and conflict ranges, prefix included
}

// track counts a conflict range of key's n bytes, which FoundationDB stores
// as both of its ends.
func (t *txn) track(n int) error {
	return t.count(n, 2*(len(t.s.prefix)+n)+1)
}

// count adds n bytes as kv counts them and size as FoundationDB does,
// refusing either past its limit.
func (t *txn) count(n, size int) error {
	if t.bytes+n > t.s.limits.TxnBytes || t.size+size > maxTxn {
		return kv.ErrTooLarge
	}
	t.bytes += n
	t.size += size
	return nil
}

// Get is a tracked read: a write of key committed after the snapshot aborts
// the transaction.
func (t *txn) Get(key []byte) ([]byte, error) {
	if err := t.track(len(key)); err != nil {
		return nil, err
	}
	if w, ok := t.own[string(key)]; ok {
		if w.Deleted {
			return nil, kv.ErrNotFound
		}
		return bytes.Clone(w.Value), nil
	}
	return t.get(t.tr, key)
}

// Guard adds key's read conflict without reading it.
//
// decision: a transaction that guards but writes nothing is not checked:
// attempt does not commit one, as FoundationDB checks no conflict of a
// read-only transaction anyway, and the interface lets a backend skip that
// check.
func (t *txn) Guard(key []byte) error {
	if err := t.track(len(key)); err != nil {
		return err
	}
	return mapErr(t.tr.AddReadConflictKey(t.s.key(key)))
}

func (t *txn) GuardRange(prefix []byte) error {
	if err := t.track(len(prefix)); err != nil {
		return err
	}
	end, err := t.s.rangeEnd(prefix)
	if err != nil {
		return err
	}
	return mapErr(t.tr.AddReadConflictRange(fdb.KeyRange{Begin: t.s.key(prefix), End: end}))
}

func (t *txn) Set(key, value []byte) error { return t.write(key, value, false) }
func (t *txn) Delete(key []byte) error     { return t.write(key, nil, true) }
func (t *txn) Now() time.Time              { return t.now }

// write records the write in the overlay. A key written twice is counted
// twice, as the conflict range and mutation each attempt adds would be once
// per key; the overcount is harmless.
func (t *txn) write(key, value []byte, deleted bool) error {
	l := t.s.limits
	if len(key) > l.Key || len(value) > l.Value {
		return kv.ErrTooLarge
	}
	full := len(t.s.prefix) + len(key)
	// The mutation, its stamp and offset, and the read conflict range on key.
	if err := t.count(len(key)+len(value), full+len(value)+stampSize+4+2*full+1); err != nil {
		return err
	}
	t.own.Put(bytes.Clone(key), bytes.Clone(value), deleted)
	return nil
}
