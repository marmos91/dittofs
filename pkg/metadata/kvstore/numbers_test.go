package kvstore

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata/kv"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

func newShare(t *testing.T, s *Store) entity.ShareID {
	t.Helper()
	sh := entity.Share{ID: entity.NewShareID(), Name: "s", Path: "/s", Root: entity.NewFileID()}
	if err := s.ControlPlane().PutShare(context.Background(), sh); err != nil {
		t.Fatal(err)
	}
	return sh.ID
}

// No number is issued twice: not by concurrent creates, and not by a later
// process over the same store, which abandons the range the first held.
func TestNumbersNeverReused(t *testing.T) {
	ctx := context.Background()
	a := openStore(t)
	share := newShare(t, a)
	seen := sync.Map{}
	var wg sync.WaitGroup
	var dup error
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range numberRange / 2 {
				n, err := a.nextNumber(ctx, share)
				if err != nil {
					dup = err
					return
				}
				if _, loaded := seen.LoadOrStore(n, true); loaded || n <= rootNumber {
					dup = errors.New("number issued twice or not above the root's")
				}
			}
		}()
	}
	wg.Wait()
	if dup != nil {
		t.Fatal(dup)
	}
	last, _ := a.nextNumber(ctx, share)

	b, err := Open(ctx, a.kv) // a second process over the same store
	if err != nil {
		t.Fatal(err)
	}
	n, err := b.nextNumber(ctx, share)
	if err != nil || n <= last {
		t.Fatalf("after reopen: %d, %v; want above %d", n, err, last)
	}
	if _, loaded := seen.Load(n); loaded {
		t.Fatalf("reissued %d", n)
	}
}

func TestNumbersBounded(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	share := newShare(t, s)
	rows(t, s, numberKey(share), encodeCounter(maxNumber-numberRange+1))
	if _, err := s.nextNumber(ctx, share); !errors.Is(err, ErrNumbersExhausted) {
		t.Fatalf("past 2^48: %v", err)
	}
}

// stallKV holds every Update until release is closed, once armed.
type stallKV struct {
	kv.KV
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *stallKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if s.armed.Load() {
		s.entered <- struct{}{}
		<-s.release
	}
	return s.KV.Update(ctx, fn)
}

// A share reserving a range of numbers, which commits to the store, holds
// up no other share's creates that have numbers in hand.
func TestNumbersReserveAlone(t *testing.T) {
	ctx := context.Background()
	st := &stallKV{entered: make(chan struct{}), release: make(chan struct{})}
	s := openStoreOver(t, func(db kv.KV) kv.KV { st.KV = db; return st })
	a := newShare(t, s)
	bs := entity.Share{ID: entity.NewShareID(), Name: "t", Path: "/t", Root: entity.NewFileID()}
	if err := s.ControlPlane().PutShare(ctx, bs); err != nil {
		t.Fatal(err)
	}
	b := bs.ID
	if _, err := s.nextNumber(ctx, b); err != nil { // b holds a range
		t.Fatal(err)
	}
	st.armed.Store(true)
	go func() { _, _ = s.nextNumber(ctx, a) }() // a reserves, and stalls
	<-st.entered
	got := make(chan error, 1)
	go func() { _, err := s.nextNumber(ctx, b); got <- err }()
	select {
	case err := <-got:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("a create on one share waited for another share's reservation")
	}
	st.armed.Store(false)
	close(st.release)
}

// openStoreOver opens a store over a fresh in-memory KV behind wrap.
func openStoreOver(t *testing.T, wrap func(kv.KV) kv.KV) *Store {
	t.Helper()
	db := wrap(memKV(t))
	if err := Init(context.Background(), db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A server-wide counter that has issued every ID says so as itself, not as
// a share out of file numbers; open IDs run out at 2^40, which with the shard
// fills an 8-byte FileId, and the other kinds at 2^64.
func TestIDsBounded(t *testing.T) {
	for _, c := range []struct {
		kind IDKind
		top  uint64
	}{{IDOpen, maxOpenID}, {IDClient, math.MaxUint64}, {IDLock, math.MaxUint64}} {
		s := openStore(t)
		rows(t, s, idKey(byte(c.kind)), encodeCounter(c.top-numberRange))
		if n, err := s.NextID(context.Background(), c.kind); err != nil || n != c.top-numberRange {
			t.Fatalf("kind %d, the last range: %d, %v", c.kind, n, err)
		}
		s = openStore(t)
		rows(t, s, idKey(byte(c.kind)), encodeCounter(c.top-numberRange+1))
		if _, err := s.NextID(context.Background(), c.kind); !errors.Is(err, ErrIDsExhausted) {
			t.Fatalf("kind %d, an exhausted counter: %v", c.kind, err)
		}
	}
}
