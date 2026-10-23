package openstate

import (
	"context"
	"crypto/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
)

var ctx = context.Background()

// fakeClock moves only when a test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Duration
	waiters []waiter
}

type waiter struct {
	at time.Duration
	ch chan struct{}
}

func (c *fakeClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan struct{})
	c.waiters = append(c.waiters, waiter{c.now + d, ch})
	return ch
}

// advance moves the clock by d and fires every timer now due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += d
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at > c.now {
			kept = append(kept, w)
		} else {
			close(w.ch)
		}
	}
	c.waiters = kept
}

// ended is one ClientEnded call.
type ended struct {
	c      entity.ClientID
	shards []ShardID
}

// closed is one Closed call.
type closed struct {
	f entity.FileID
	r CloseResult
}

// waitOverCall is one LockWaitOver call.
type waitOverCall struct {
	w       LockWaiter
	granted bool
}

// recallCall is one RecallGrant call.
type recallCall struct {
	g  CachingGrant
	to GrantKind
}

// recordingSink records what the table raises; onEnded, onWait and onRecall,
// if set, run inside ClientEnded, LockWaitOver and RecallGrant.
type recordingSink struct {
	mu       sync.Mutex
	ended    []ended
	closed   []closed
	waits    []waitOverCall
	recalls  []recallCall
	revoked  []Revocation
	onEnded  func(c entity.ClientID)
	onWait   func(w LockWaiter)
	onRecall func(g CachingGrant, to GrantKind)
}

func (s *recordingSink) RecallGrant(_ entity.ClientID, g CachingGrant, to GrantKind) {
	s.mu.Lock()
	s.recalls = append(s.recalls, recallCall{g, to})
	fn := s.onRecall
	s.mu.Unlock()
	if fn != nil {
		fn(g, to)
	}
}

func (s *recordingSink) Revoked(r Revocation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = append(s.revoked, r)
}

// takeRecalls returns the RecallGrant calls since the last take.
func (s *recordingSink) takeRecalls() []recallCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recalls
	s.recalls = nil
	return r
}

// takeRevoked returns the Revoked calls since the last take.
func (s *recordingSink) takeRevoked() []Revocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.revoked
	s.revoked = nil
	return r
}

func (s *recordingSink) LockWaitOver(w LockWaiter, granted bool) {
	s.mu.Lock()
	s.waits = append(s.waits, waitOverCall{w, granted})
	fn := s.onWait
	s.mu.Unlock()
	if fn != nil {
		fn(w)
	}
}

// takeWaits returns the LockWaitOver calls since the last take.
func (s *recordingSink) takeWaits() []waitOverCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waits
	s.waits = nil
	return w
}

// orderLog is an Order that records each file it is taken for.
type orderLog struct {
	mu    sync.Mutex
	files []entity.FileID
}

func (l *orderLog) order(f entity.FileID) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.files = append(l.files, f)
	return func() {}
}

func (s *recordingSink) Closed(f entity.FileID, r CloseResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, closed{f, r})
}

func (s *recordingSink) ClientEnded(c entity.ClientID, shards []ShardID, _ RevokeReason) {
	s.mu.Lock()
	s.ended = append(s.ended, ended{c, shards})
	fn := s.onEnded
	s.mu.Unlock()
	if fn != nil {
		fn(c)
	}
}

func (s *recordingSink) endedOf(c entity.ClientID) []ended {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.ended), func(e ended) bool { return e.c != c })
}

// fixture is a started table over a fresh in-memory store, with the store's
// write counts, the clock the table reads and the Sink it raises events to.
type fixture struct {
	*Table
	Clients
	fs    *kvstoretest.FS
	clock *fakeClock
	sink  *recordingSink
	order *orderLog
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		return db
	})
	return reopen(t, fs, &fakeClock{})
}

// reopen builds and starts a new table over fs's store, as a restart does:
// the store keeps what is durable, and the table starts empty.
func reopen(t *testing.T, fs *kvstoretest.FS, clock *fakeClock) *fixture {
	t.Helper()
	f := unstarted(t, fs, clock)
	if err := f.Start(ctx, f.sink, f.order.order); err != nil {
		t.Fatal(err)
	}
	return f
}

func unstarted(t *testing.T, fs *kvstoretest.FS, clock *fakeClock) *fixture {
	t.Helper()
	tab, err := New(Deps{Records: fs.Store, Clock: clock, Rand: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tab.Stop)
	return &fixture{Table: tab, Clients: tab.Clients(), fs: fs, clock: clock, sink: &recordingSink{}, order: &orderLog{}}
}
