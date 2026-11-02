package segment

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// TestConcurrentRetireSubtractsOnce: many goroutines retire the same segment
// at once, and exactly one of them — the one that found it in the set —
// subtracts: the membership test and the subtraction must be one step under
// the ledger's mutex, with no check-then-act across a release. The losses
// are counted, and the same race between a retire and a recycle over one id
// has exactly one winner.
func TestConcurrentRetireSubtractsOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	const rounds = 50
	const racers = 8

	for round := range rounds {
		var logs capturing
		s := NewSet(segCap, newLogger(&logs))
		id := core.SegmentID(1)
		if err := s.AddSpare(id); err != nil {
			t.Fatalf("AddSpare: %v", err)
		}
		if err := s.Take(id); err != nil {
			t.Fatalf("Take: %v", err)
		}
		if err := s.Seal(id); err != nil {
			t.Fatalf("Seal: %v", err)
		}

		var wg sync.WaitGroup
		var winners, losers sync.WaitGroup
		winners.Add(1)
		losers.Add(racers - 1)
		for range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.Retire(id)
				switch {
				case err == nil:
					winners.Done()
				case errors.Is(err, ErrNotInSet):
					losers.Done()
				default:
					t.Errorf("a concurrent retire returned %v, want nil or %v", err, ErrNotInSet)
					winners.Done()
					losers.Done()
				}
			}()
		}
		wg.Wait()

		c := s.Counters()
		if c.UsedBytes != 0 {
			t.Fatalf("round %d: the footprint after %d concurrent retires is %d, want 0", round, racers, c.UsedBytes)
		}
		if c.DoubleRetires != racers-1 {
			t.Fatalf("round %d: %d double retires were counted, want %d", round, c.DoubleRetires, racers-1)
		}
		if len(logs.recs) != racers-1 {
			t.Errorf("round %d: %d Warn records were logged, want %d", round, len(logs.recs), racers-1)
		}
		if err := s.Check(); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}

		// The same race between a retire and a recycle over one id: exactly
		// one of them moves the segment out of the set.
		s2 := NewSet(segCap, nil) // this half counts errors, not log records
		if err := s2.AddSpare(id); err != nil {
			t.Fatalf("AddSpare: %v", err)
		}
		if err := s2.Take(id); err != nil {
			t.Fatalf("Take: %v", err)
		}
		if err := s2.Seal(id); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		var (
			wg2      sync.WaitGroup
			winners2 int32
			refused2 int32
		)
		for i := range 2 {
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				var err error
				if i == 0 {
					_, err = s2.Retire(id)
				} else {
					err = s2.Recycle(id, core.SegmentID(2))
				}
				if err == nil {
					atomic.AddInt32(&winners2, 1)
				} else {
					atomic.AddInt32(&refused2, 1)
				}
			}()
		}
		wg2.Wait()
		if w, r := atomic.LoadInt32(&winners2), atomic.LoadInt32(&refused2); w != 1 || r != 1 {
			t.Fatalf("round %d: the retire and the recycle ended %d to %d, want exactly one winner", round, w, r)
		}
		if err := s2.Check(); err != nil {
			t.Fatalf("round %d, retire against recycle: %v", round, err)
		}
	}
}

// newLogger wraps a handler in the logger the ledger takes.
func newLogger(h *capturing) *slog.Logger { return slog.New(h) }
