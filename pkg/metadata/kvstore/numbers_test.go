package kvstore

import (
	"context"
	"errors"
	"sync"
	"testing"

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
