// Package badgerkv is the embedded single-node kv backend, on Badger.
package badgerkv

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
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
// Update returns.
//
// ponytail: a commit costs one device sync of its own, and Badger shares a
// sync between concurrent commits only by chance, so the store commits about
// 1/sync-latency transactions per second in all (~400 on consumer NVMe).
// Group concurrent Updates into one sync when a profile shows commits queued
// on the sync.
type Store struct {
	db     *badger.DB
	clk    []byte // this node's clock-ceiling key
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
	bo := badger.DefaultOptions(opts.Dir).WithLogger(nil)
	if opts.Dir == "" {
		bo = bo.WithInMemory(true)
	} else {
		bo = bo.WithSyncWrites(true)
	}
	db, err := badger.Open(bo)
	if err != nil {
		return nil, err
	}
	s := &Store{
		db:    db,
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
	s.limits.Entries = int(db.MaxBatchCount()) - 2
	s.limits.Bytes = int(db.MaxBatchSize()) - 1 - markerSize - entryOverhead*s.limits.Entries
	s.limits.Key = maxKeySize
	s.limits.Value = min(int(bo.ValueLogFileSize), s.limits.Bytes-1)
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
		db.Close()
		return nil, err
	}
	if err := s.raiseCeiling(); err != nil {
		db.Close()
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

func (s *Store) Limits() kv.TxnLimits { return s.limits }

func (s *Store) Close() error {
	close(s.stop)
	<-s.done
	return s.db.Close()
}

func (s *Store) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return kv.Retry(ctx, func() error { return s.attempt(fn) })
}

func (s *Store) View(_ context.Context, fn func(kv.Reader) error) error {
	r := s.db.NewTransaction(false)
	defer r.Discard()
	return fn(&reader{snap: r})
}

func (s *Store) attempt(fn func(kv.Txn) error) error {
	t := &txn{s: s, reader: reader{own: map[string]write{}}, now: s.now()}
	t.w, t.snap = s.snapshotPair()
	defer t.w.Discard()
	defer t.snap.Discard()
	if err := fn(t); err != nil {
		return err
	}
	done := make(chan error, 1)
	s.commitMu.Lock()
	if err := s.checkRanges(t); err != nil {
		s.commitMu.Unlock()
		return err
	}
	// CommitWith checks conflicts and takes the commit timestamp before it
	// returns, then syncs in the background, so the lock covers no sync.
	t.w.CommitWith(func(err error) { done <- err })
	s.commitMu.Unlock()
	if err := <-done; err != nil {
		if errors.Is(err, badger.ErrConflict) {
			return kv.ErrConflict
		}
		return err
	}
	return nil
}

// checkRanges fails with kv.ErrConflict if any transaction committed a write
// under one of t's guarded ranges after t's snapshot. Badger tracks no range,
// so the store does: a snapshot opened now waits for every commit that has
// passed its conflict check, and under commitMu no other commit can pass one
// before t's does.
//
// ponytail: a range-guarded commit holds commitMu while the fresh snapshot
// waits for the commits ahead of it to sync, stalling every other commit for
// up to one sync, and it reads every version under its ranges. Both are
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
			seq := binary.BigEndian.AppendUint64(nil, item.Version())
			if !yield(kv.KeyValue{Key: item.Key(), Value: v, Seq: seq}, nil) {
				return
			}
			it.Next()
		}
	}
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
func (t *txn) Get(key []byte) ([]byte, error) { return get(t.w, key) }

// Guard is a tracked read whose value is dropped. Badger tracks a key read
// even when it is absent, so a guard of a missing key conflicts with its
// create.
//
// decision: a transaction that guards but writes nothing is not checked:
// Badger commits a read-only transaction without a conflict check, as
// FoundationDB does. Such a transaction changes nothing a guard protects, but
// its guard is a snapshot read, not a check at commit. Write a key in the same
// transaction if a caller ever has to act on a guard that only reads.
func (t *txn) Guard(key []byte) error {
	_, err := t.Get(key)
	if errors.Is(err, kv.ErrNotFound) {
		return nil
	}
	return err
}

func (t *txn) GuardRange(prefix []byte) error {
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
// and one write would be lost.
func (t *txn) write(key, value []byte, deleted bool) error {
	if err := t.Guard(key); err != nil {
		return err
	}
	l := t.s.limits
	if len(key) > l.Key || len(value) > l.Value ||
		t.count+1 > l.Entries || t.bytes+len(key)+len(value) > l.Bytes {
		return kv.ErrTooLarge
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
