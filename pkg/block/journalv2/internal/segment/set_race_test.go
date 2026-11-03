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
		s := NewSet(segCap, slog.New(&logs))
		id := core.SegmentID(1)
		if err := s.AddSpare(id, noMax); err != nil {
			t.Fatalf("AddSpare: %v", err)
		}
		if err := s.Take(id); err != nil {
			t.Fatalf("Take: %v", err)
		}
		if err := s.Seal(id, true); err != nil {
			t.Fatalf("Seal: %v", err)
		}

		var wg sync.WaitGroup
		var winners, losers atomic.Int32
		for range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				freed, err := s.Retire(id)
				switch {
				case err == nil && freed == segCap:
					winners.Add(1)
				case errors.Is(err, ErrNotInSet) && freed == 0:
					losers.Add(1)
				default:
					t.Errorf("a concurrent retire returned %d, %v, want the cap and nil, or 0 and %v", freed, err, ErrNotInSet)
				}
			}()
		}
		wg.Wait()
		if w, l := winners.Load(), losers.Load(); w+l != racers || w != 1 || l != racers-1 {
			t.Fatalf("round %d: %d retires subtracted and %d found nothing, want 1 and %d", round, w, l, racers-1)
		}

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
		if err := s2.AddSpare(id, noMax); err != nil {
			t.Fatalf("AddSpare: %v", err)
		}
		if err := s2.Take(id); err != nil {
			t.Fatalf("Take: %v", err)
		}
		if err := s2.Seal(id, true); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		var wg2 sync.WaitGroup
		var retired, recycled, refused2 atomic.Int32
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
				if err != nil {
					refused2.Add(1)
					return
				}
				if i == 0 {
					retired.Add(1)
				} else {
					recycled.Add(1)
				}
			}()
		}
		wg2.Wait()
		// The loser is refused whichever side wins, so the flags name the
		// winner by kind, not by count: one retired or one recycled, never
		// both, never neither.
		if w, r := retired.Load(), recycled.Load(); w+r != 1 || refused2.Load() != 1 {
			t.Fatalf("round %d: %d retires and %d recycles won, %d refused, want one winner of one kind and one refusal", round, w, r, refused2.Load())
		}
		c2 := s2.Counters()
		if c2.Segments != 0 {
			t.Fatalf("round %d, retire against recycle: the set still holds %d segments, want the one id gone whichever call took it", round, c2.Segments)
		}
		if recycleWon := recycled.Load() == 1; !recycleWon && c2.UsedBytes != 0 || recycleWon && c2.UsedBytes != segCap {
			t.Fatalf("round %d, retire against recycle: the recycle won %v, but the footprint is %d with %d spares", round, recycleWon, c2.UsedBytes, c2.Spares)
		}
		if recycled.Load() == 1 && c2.Spares != 1 {
			t.Fatalf("round %d: the recycle won but left %d spares, want one", round, c2.Spares)
		}
		if retired.Load() == 1 && c2.Spares != 0 {
			t.Fatalf("round %d: the retire won but left %d spares, want none", round, c2.Spares)
		}
		if err := s2.Check(); err != nil {
			t.Fatalf("round %d, retire against recycle: %v", round, err)
		}
	}
}

// TestConcurrentAddSpareAtTheMaximum: 64 goroutines reserve distinct spares at
// once against a maximum that holds exactly 10 of them; the reservation and
// the check of the maximum are one critical section under the ledger's mutex,
// so exactly 10 succeed and every other call is refused by the journal limit
// with the footprint at the maximum and the ledger consistent — a make that
// read Counters() first and decided after would let two through.
func TestConcurrentAddSpareAtTheMaximum(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	const makers = 64
	const kept = 10

	s := NewSet(segCap, nil)
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := range makers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.AddSpare(core.SegmentID(i+1), kept*segCap)
			switch {
			case err == nil:
				wins.Add(1)
			default:
				var ns *core.NoSpaceError
				if !errors.As(err, &ns) || ns.Limit != core.LimitJournal {
					t.Errorf("a refused AddSpare returned %v, want a *core.NoSpaceError naming the journal limit", err)
				}
			}
		}()
	}
	wg.Wait()

	if wins.Load() != kept {
		t.Fatalf("%d reservations passed the maximum, want exactly %d", wins.Load(), kept)
	}
	c := s.Counters()
	if c.UsedBytes != kept*segCap {
		t.Fatalf("after the reservations, the footprint is %d, want exactly the maximum %d", c.UsedBytes, kept*segCap)
	}
	if c.Spares != kept {
		t.Fatalf("after the reservations, the ledger holds %d spares, want %d", c.Spares, kept)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("after the reservations, %v", err)
	}
}
