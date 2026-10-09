package badgerkv

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/kvtest"
)

func open(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.KV {
		s := open(t, Options{})
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// The suite on disk, with every commit synced: the backend as it ships.
func TestConformanceOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("syncs every commit")
	}
	kvtest.Run(t, func(t *testing.T) kv.KV {
		s := open(t, Options{Dir: t.TempDir()})
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// A returned Update reads back after a reopen. A process restart does not
// lose the page cache, so this proves the commit reached the store, not the
// device; a power-loss test needs a harness that drops the cache.
func TestCommitSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, Options{Dir: dir})
	if err := s.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Set([]byte("k"), []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, Options{Dir: dir})
	defer s.Close()
	if err := s.View(context.Background(), func(r kv.Reader) error {
		v, err := r.Get([]byte("k"))
		if err == nil && string(v) != "v" {
			t.Errorf("got %q", v)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func txnNow(t *testing.T, s *Store) time.Time {
	t.Helper()
	var now time.Time
	if err := s.Update(context.Background(), func(tx kv.Txn) error {
		now = tx.Now()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return now
}

func TestNowAcrossRestartWithClockSteppedBack(t *testing.T) {
	dir := t.TempDir()
	start := time.Unix(1_800_000_000, 0)
	clock := &fakeClock{t: start}
	s := open(t, Options{Dir: dir, Clock: clock.now})
	before := txnNow(t, s)
	s.Close()

	clock.set(start.Add(-time.Hour))
	s = open(t, Options{Dir: dir, Clock: clock.now})
	defer s.Close()
	if after := txnNow(t, s); after.Before(before) {
		t.Fatalf("Now went from %v to %v across a restart", before, after)
	}
}

func TestNowStaysUnderCeiling(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	clock := &fakeClock{t: start}
	s := open(t, Options{Clock: clock.now})
	defer s.Close()

	// The clock jumps past the ceiling before the next raise: Now holds at
	// the ceiling rather than pass it.
	clock.set(start.Add(time.Hour))
	if now, limit := txnNow(t, s), start.Add(ceilingAhead); now.After(limit) {
		t.Fatalf("Now %v passed the ceiling %v", now, limit)
	}
	// Once raised, Now follows the clock again.
	if err := s.raiseCeiling(); err != nil {
		t.Fatal(err)
	}
	if now := txnNow(t, s); !now.Equal(start.Add(time.Hour)) {
		t.Fatalf("Now %v after the raise, want the clock's %v", now, start.Add(time.Hour))
	}
}
