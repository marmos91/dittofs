// Package badgerkv is the embedded single-node kv backend, on Badger.
package badgerkv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Options configures a Store.
type Options struct {
	// Dir holds the store. Empty keeps it in memory, which loses everything at
	// Close and is meant for tests.
	Dir string
	// NodeID names this node's clock record, N‖node‖clk.
	NodeID [keys.IDLen]byte
	// Clock is the node's clock; nil is time.Now.
	Clock func() time.Time
}

// Store is a kv.KV on Badger. Every commit is synced to the device before
// Update returns, and concurrent commits share one sync (syncer): Badger's
// own sync is off, because it syncs its memtable WAL once per transaction even
// when it writes several in one batch. No snapshot sees a commit before it is
// durable either: a View, or an Update that returns having read one, waits for
// its sync.
type Store struct {
	db     *badger.DB
	sync   *syncer // nil in memory
	clk    []byte  // this node's clock-ceiling key
	clock  func() time.Time
	limits kv.TxnLimits

	// commitMu orders every commit's conflict check, so a range guard's
	// check and the commit it admits see no other commit between them.
	commitMu sync.Mutex

	mu      sync.Mutex
	last    time.Time // the latest Now handed out
	ceiling time.Time // Now never passes it

	stop chan struct{}
	done chan struct{}

	// gate is held shared by every Update and View attempt and exclusively
	// by Close, which so waits for the calls in flight before it stops the
	// syncer they wait on. closed is set under it.
	gate   sync.RWMutex
	closed bool
}

// ceilingAhead is how far ahead of the clock the ceiling is raised, once per
// ceilingPeriod. Twice the period, so Now reaches the ceiling only when a
// raise has failed or stalled for a whole period.
const (
	ceilingPeriod = time.Second
	ceilingAhead  = 2 * ceilingPeriod
)

// Open opens or creates the store and raises its clock ceiling before
// returning, so no Now precedes a durable ceiling.
func Open(opts Options) (*Store, error) {
	bo := badger.DefaultOptions(opts.Dir).WithLogger(nil).WithSyncWrites(false)
	if opts.Dir == "" {
		bo = bo.WithInMemory(true)
	}
	db, err := badger.Open(bo)
	if err != nil {
		return nil, err
	}
	var y *syncer
	if !bo.InMemory {
		if y, err = newSyncer(db, opts.Dir); err != nil {
			_ = db.Close() // the open already failed; that error is the one returned
			return nil, err
		}
	}
	s := &Store{
		db:    db,
		sync:  y,
		clk:   append(keys.ID([]byte{keys.KindN}, opts.NodeID[:]), keys.NClk),
		clock: opts.Clock,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	// Badger refuses a transaction whose estimated size reaches its batch
	// limits, counting one entry and markerSize bytes for its commit marker
	// and estimating each entry at no more than its key and value plus
	// entryOverhead bytes. Declaring the limits net of that overhead keeps
	// every transaction Limits admits inside Badger's.
	// TxnBytes counts conflict ranges too, which Badger does not, so it only
	// adds to what Badger's own estimate counts.
	s.limits.TxnEntries = int(db.MaxBatchCount()) - 2
	s.limits.TxnBytes = int(db.MaxBatchSize()) - 1 - markerSize - entryOverhead*s.limits.TxnEntries
	s.limits.Key = maxKeySize
	s.limits.Value = min(int(bo.ValueLogFileSize), s.limits.TxnBytes-1)
	s.limits.Age = maxAge
	if bo.InMemory {
		// Badger admits a value of exactly ValueThreshold in memory, then
		// panics writing it, since only a smaller one skips the value log.
		s.limits.Value = int(bo.ValueThreshold) - 1
	}

	// A start resumes from the stored ceiling, so a clock stepped back across
	// a restart cannot carry Now backward.
	if err := db.View(func(t *badger.Txn) error {
		c, err := readCeiling(t, s.clk)
		s.last, s.ceiling = c, c
		return err
	}); err != nil {
		s.closeSync()
		_ = db.Close() // the open already failed; that error is the one returned
		return nil, err
	}
	if err := s.raiseCeiling(); err != nil {
		s.closeSync()
		_ = db.Close() // the open already failed; that error is the one returned
		return nil, err
	}
	go s.ceilingLoop()
	return s, nil
}

const (
	entryOverhead = 12
	maxKeySize    = 65000 // Badger's
	markerSize    = 64
)

// decision: Badger holds a snapshot for as long as a transaction lives, so it
// sets no age of its own; this one is the replicated store's five seconds, so
// a batch sized and timed on the embedded store also commits on that one.
// Raise it only together with that backend's.
const maxAge = 5 * time.Second

func (s *Store) Limits() kv.TxnLimits { return s.limits }

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
	s.closeSync()
	return s.db.Close()
}

// enter admits one call, refusing it once the store is closed; the caller
// runs leave when it is done.
func (s *Store) enter() error {
	s.gate.RLock()
	if s.closed {
		s.gate.RUnlock()
		return kv.ErrClosed
	}
	return nil
}

func (s *Store) leave() { s.gate.RUnlock() }

func (s *Store) closeSync() {
	if s.sync != nil {
		s.sync.close()
	}
}

// durable returns once every commit at or below version ts is durable.
func (s *Store) durable(ts uint64) error {
	if s.sync == nil || ts <= s.sync.synced.Load() {
		return nil
	}
	return s.sync.wait()
}

func (s *Store) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return kv.Retry(ctx, func() error { return s.attempt(fn) })
}

func (s *Store) View(_ context.Context, fn func(kv.Reader) error) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	r := s.db.NewTransaction(false)
	defer r.Discard()
	if err := s.durable(r.ReadTs()); err != nil {
		return err
	}
	return fn(&reader{snap: r})
}

func (s *Store) attempt(fn func(kv.Txn) error) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	t := &txn{s: s, reader: reader{own: map[string]write{}}, now: s.now()}
	t.w, t.snap = s.snapshotPair()
	start := time.Now()
	defer t.w.Discard()
	defer t.snap.Discard()
	if err := fn(t); err != nil {
		// What fn read decided its error, so the error waits as a result would.
		if derr := s.durable(t.snap.ReadTs()); derr != nil {
			return errors.Join(err, derr)
		}
		return err
	}
	if age := time.Since(start); age > s.limits.Age {
		return fmt.Errorf("%w: transaction open %v, past its age limit of %v", kv.ErrConflict, age, s.limits.Age)
	}
	done := make(chan error, 1)
	s.commitMu.Lock()
	if err := s.checkRanges(t); err != nil {
		s.commitMu.Unlock()
		return err
	}
	// CommitWith checks conflicts and takes the commit timestamp before it
	// returns, then writes in the background, so the lock covers no write.
	t.w.CommitWith(func(err error) { done <- err })
	s.commitMu.Unlock()
	if err := <-done; err != nil {
		if errors.Is(err, badger.ErrConflict) {
			return kv.ErrConflict
		}
		return err
	}
	if t.count == 0 {
		return s.durable(t.snap.ReadTs())
	}
	// Written, not yet durable: the next shared sync covers it.
	return s.durable(math.MaxUint64)
}

// checkRanges fails with kv.ErrConflict if any transaction committed a write
// under one of t's guarded ranges after t's snapshot. Badger tracks no range,
// so the store does: a snapshot opened now waits for every commit that has
// passed its conflict check, and under commitMu no other commit can pass one
// before t's does.
//
// ponytail: a range-guarded commit holds commitMu while the fresh snapshot
// waits for the commits ahead of it to be written, which the store does not
// sync first, stalling every other commit that long, and it reads every
// version under its ranges. Both are
// cheap while range guards stay rare (a directory removal); track ranges in
// memory when a workload guards ranges often enough to show in a profile.
func (s *Store) checkRanges(t *txn) error {
	if len(t.ranges) == 0 {
		return nil
	}
	now := s.db.NewTransaction(false)
	defer now.Discard()
	opts := badger.DefaultIteratorOptions
	opts.AllVersions = true
	opts.PrefetchValues = false
	for _, prefix := range t.ranges {
		opts.Prefix = prefix
		it := now.NewIterator(opts)
		for it.Seek(prefix); it.Valid(); it.Next() {
			if it.Item().Version() > t.snap.ReadTs() {
				it.Close()
				return kv.ErrConflict
			}
		}
		it.Close()
	}
	return nil
}

// snapshotPair opens a read-write transaction and a read-only one on the same
// snapshot. Badger tracks every key an iterator in a read-write transaction
// visits, which would make a scan conflict with any write under its range, so
// scans read the read-only twin and merge the transaction's own writes.
func (s *Store) snapshotPair() (w, r *badger.Txn) {
	for {
		w = s.db.NewTransaction(true)
		r = s.db.NewTransaction(false)
		if w.ReadTs() == r.ReadTs() {
			return w, r
		}
		// A commit landed between the two; try again.
		w.Discard()
		r.Discard()
	}
}

// now is the node clock clamped monotone: never below the last value handed
// out and never past the ceiling. Its bound of real time is ceilingAhead ahead
// after a restart, and behind by however long a ceiling raise stalls.
func (s *Store) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.clock()
	if t.Before(s.last) {
		t = s.last
	}
	if t.After(s.ceiling) {
		t = s.ceiling
	}
	s.last = t
	return t
}

func (s *Store) ceilingLoop() {
	defer close(s.done)
	tick := time.NewTicker(ceilingPeriod)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			// A failed raise is retried next period; meanwhile Now holds at
			// the old ceiling rather than pass it.
			_ = s.raiseCeiling()
		}
	}
}

// raiseCeiling writes the ceiling ceilingAhead past the clock, and only then
// lets Now use it. The ceiling is its own key, so raising it conflicts with
// no other transaction's writes.
func (s *Store) raiseCeiling() error {
	s.mu.Lock()
	c := s.clock().Add(ceilingAhead)
	if c.Before(s.ceiling) {
		c = s.ceiling
	}
	s.mu.Unlock()
	v := codec.NewWriter(1).Uint64(uint64(c.UnixNano())).Encode()
	// Through Update, so this commit is ordered by commitMu like every other.
	if err := s.Update(context.Background(), func(t kv.Txn) error { return t.Set(s.clk, v) }); err != nil {
		return err
	}
	s.mu.Lock()
	if c.After(s.ceiling) {
		s.ceiling = c
	}
	s.mu.Unlock()
	return nil
}

func readCeiling(t *badger.Txn, key []byte) (time.Time, error) {
	item, err := t.Get(key)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	b, err := item.ValueCopy(nil)
	if err != nil {
		return time.Time{}, err
	}
	r, _, err := codec.NewReader(b, 1)
	if err != nil {
		return time.Time{}, fmt.Errorf("clock ceiling: %w", err)
	}
	ns := r.Uint64()
	if err := r.Done(); err != nil {
		return time.Time{}, fmt.Errorf("clock ceiling: %w", err)
	}
	return time.Unix(0, int64(ns)), nil
}

// reader reads one snapshot.
type reader struct {
	snap *badger.Txn
	own  map[string]write // the transaction's own writes; nil in a View
}

type write struct {
	value   []byte
	deleted bool
}

func (r *reader) Get(key []byte) ([]byte, error) {
	return get(r.snap, key)
}

func get(t *badger.Txn, key []byte) ([]byte, error) {
	item, err := t.Get(key)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, kv.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return item.ValueCopy(nil)
}

// ponytail: each Scan inside Update sorts the transaction's own writes under
// its prefix, O(w log w) for w writes. Keep them in an ordered structure when
// a transaction that writes and scans many keys shows it in a profile.
func (r *reader) Scan(prefix, after []byte) iter.Seq2[kv.KeyValue, error] {
	return func(yield func(kv.KeyValue, error) bool) {
		var mine []string
		for k := range r.own {
			if bytes.HasPrefix([]byte(k), prefix) && (after == nil || k > string(after)) {
				mine = append(mine, k)
			}
		}
		slices.Sort(mine)

		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := r.snap.NewIterator(opts)
		defer it.Close()
		start := prefix
		if bytes.Compare(after, prefix) > 0 {
			start = after
		}
		it.Seek(start)
		if it.Valid() && after != nil && bytes.Equal(it.Item().Key(), after) {
			it.Next()
		}

		for it.Valid() || len(mine) > 0 {
			var stored []byte
			if it.Valid() {
				stored = it.Item().Key()
			}
			if len(mine) > 0 && (stored == nil || mine[0] <= string(stored)) {
				k := mine[0]
				mine = mine[1:]
				if stored != nil && k == string(stored) {
					it.Next()
				}
				if w := r.own[k]; !w.deleted && !yield(kv.KeyValue{Key: []byte(k), Value: w.value}, nil) {
					return
				}
				continue
			}
			item := it.Item()
			v, err := item.ValueCopy(nil)
			if err != nil {
				yield(kv.KeyValue{}, err)
				return
			}
			if !yield(kv.KeyValue{Key: item.Key(), Value: v}, nil) {
				return
			}
			it.Next()
		}
	}
}

// Before merges a reverse walk of the snapshot with the transaction's own
// writes below key: the larger key wins, and an own deletion hides the
// stored key it names.
//
// ponytail: inside Update it sorts the transaction's own writes under prefix
// on every call, as Scan does. Keep them ordered when a transaction that
// writes and seeks many keys shows it in a profile.
func (r *reader) Before(prefix, key []byte) (kv.KeyValue, bool, error) {
	var mine []string
	for k := range r.own {
		if bytes.HasPrefix([]byte(k), prefix) && k < string(key) {
			mine = append(mine, k)
		}
	}
	slices.Sort(mine)

	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	opts.Reverse = true
	opts.PrefetchValues = false
	it := r.snap.NewIterator(opts)
	defer it.Close()
	it.Seek(key)
	if it.Valid() && bytes.Equal(it.Item().Key(), key) {
		it.Next()
	}
	for it.Valid() || len(mine) > 0 {
		var stored []byte
		if it.Valid() {
			stored = it.Item().Key()
		}
		if n := len(mine); n > 0 && (stored == nil || mine[n-1] >= string(stored)) {
			k := mine[n-1]
			mine = mine[:n-1]
			if stored != nil && k == string(stored) {
				it.Next()
			}
			if w := r.own[k]; !w.deleted {
				return kv.KeyValue{Key: []byte(k), Value: w.value}, true, nil
			}
			continue
		}
		item := it.Item()
		v, err := item.ValueCopy(nil)
		if err != nil {
			return kv.KeyValue{}, false, err
		}
		return kv.KeyValue{Key: item.KeyCopy(nil), Value: v}, true, nil
	}
	return kv.KeyValue{}, false, nil
}

// Versions reads the snapshot, untracked, and refuses a key the transaction
// wrote: its version is the commit's, which Badger assigns only then.
func (r *reader) Versions(keys [][]byte) ([]uint64, error) {
	vs := make([]uint64, len(keys))
	for i, k := range keys {
		if _, ok := r.own[string(k)]; ok {
			return nil, kv.ErrUncommittedVersion
		}
		item, err := r.snap.Get(k)
		if errors.Is(err, badger.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		vs[i] = item.Version()
	}
	return vs, nil
}

// txn is one attempt of an Update.
type txn struct {
	reader
	s            *Store
	w            *badger.Txn
	now          time.Time
	count, bytes int
	ranges       [][]byte
}

// Get reads through the read-write transaction, which tracks the key and
// sees the transaction's own writes.
func (t *txn) Get(key []byte) ([]byte, error) {
	if err := t.track(len(key)); err != nil {
		return nil, err
	}
	return get(t.w, key)
}

// track counts n bytes of conflict range toward TxnBytes.
func (t *txn) track(n int) error {
	if t.bytes+n > t.s.limits.TxnBytes {
		return kv.ErrTooLarge
	}
	t.bytes += n
	return nil
}

// Guard is a tracked read whose value is dropped. Badger tracks a key read
// even when it is absent, so a guard of a missing key conflicts with its
// create.
//
// decision: a transaction that guards but writes nothing is not checked:
// Badger commits a read-only transaction without a conflict check, as
// FoundationDB does, and the interface lets a backend skip that check. Such a
// transaction changes nothing a guard protects. Check it at commit if a caller
// ever has to act on a guard in a transaction that only reads.
func (t *txn) Guard(key []byte) error {
	_, err := t.Get(key)
	if errors.Is(err, kv.ErrNotFound) {
		return nil
	}
	return err
}

func (t *txn) GuardRange(prefix []byte) error {
	if err := t.track(len(prefix)); err != nil {
		return err
	}
	t.ranges = append(t.ranges, bytes.Clone(prefix))
	return nil
}

func (t *txn) Set(key, value []byte) error {
	return t.write(key, value, false)
}

func (t *txn) Delete(key []byte) error {
	return t.write(key, nil, true)
}

// write reads the key first: Badger checks only read keys against later
// commits, so without the read two blind writers of one key would both commit
// and one write would be lost. That read is the write's own conflict range,
// counted once with the write.
func (t *txn) write(key, value []byte, deleted bool) error {
	l := t.s.limits
	if len(key) > l.Key || len(value) > l.Value ||
		t.count+1 > l.TxnEntries || t.bytes+len(key)+len(value) > l.TxnBytes {
		return kv.ErrTooLarge
	}
	// The read registers the key for Badger's conflict check, which its Set
	// does not, so two blind writers of one key conflict; the value it
	// would copy is not needed.
	if _, err := t.w.Get(key); err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
		return err
	}
	key, value = bytes.Clone(key), bytes.Clone(value)
	var err error
	if deleted {
		err = t.w.Delete(key)
	} else {
		err = t.w.Set(key, value)
	}
	if err != nil {
		return err
	}
	t.count++
	t.bytes += len(key) + len(value)
	t.own[string(key)] = write{value: value, deleted: deleted}
	return nil
}

func (t *txn) Now() time.Time { return t.now }
