package segment

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// capturing is a slog Handler that keeps every record handed to it, so a test
// can assert what the ledger logged, at which level and carrying which
// attributes. Many goroutines log their own double retire at once, so the
// records are kept under a mutex of their own.
type capturing struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *capturing) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturing) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}

func (c *capturing) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *capturing) WithGroup(string) slog.Handler { return c }

// attr returns the value the record carries for key, or nil when it has none.
func (c *capturing) attr(rec slog.Record, key string) any {
	var val any
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val = a.Value.Any()
			return false
		}
		return true
	})
	return val
}

// TestRetireTwiceSubtractsOnce: a segment's bytes leave the accounted
// footprint exactly once, in the retire that removed it from the set; a
// second retire finds nothing, subtracts nothing, is counted as a double
// retire and logged at Warn naming the segment.
func TestRetireTwiceSubtractsOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	var logs capturing
	s := NewSet(segCap, slog.New(&logs))
	id := core.SegmentID(1)

	for _, op := range []struct {
		name string
		fn   func() error
	}{
		{"AddSpare", func() error { return s.AddSpare(id) }},
		{"Take", func() error { return s.Take(id) }},
		{"Seal", func() error { return s.Seal(id) }},
	} {
		if err := op.fn(); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
	}
	before := s.Counters()
	if before.UsedBytes != segCap {
		t.Fatalf("after the take, the footprint is %d, want the cap %d", before.UsedBytes, segCap)
	}

	freed, err := s.Retire(id)
	if err != nil {
		t.Fatalf("the first retire: %v", err)
	}
	if freed != segCap {
		t.Fatalf("the first retire freed %d, want %d", freed, segCap)
	}
	after := s.Counters()
	if after.UsedBytes != 0 {
		t.Fatalf("after the first retire, the footprint is %d, want 0", after.UsedBytes)
	}

	freed, err = s.Retire(id)
	if !errors.Is(err, ErrNotInSet) {
		t.Fatalf("the second retire: %v, want %v", err, ErrNotInSet)
	}
	if freed != 0 {
		t.Fatalf("the second retire freed %d, want 0", freed)
	}
	final := s.Counters()
	if final.UsedBytes != 0 {
		t.Fatalf("the second retire moved the footprint to %d, want it still 0", final.UsedBytes)
	}
	if final.DoubleRetires != 1 {
		t.Fatalf("the second retire was counted %d times, want 1", final.DoubleRetires)
	}
	if len(logs.recs) != 1 {
		t.Fatalf("the ledger logged %d records, want the one Warn", len(logs.recs))
	}
	rec := logs.recs[0]
	if rec.Level != slog.LevelWarn {
		t.Fatalf("the double retire logged at %v, want Warn", rec.Level)
	}
	if rec.Message != absentMessage {
		t.Fatalf("the double retire logged %q, want %q", rec.Message, absentMessage)
	}
	if got := logs.attr(rec, "segment"); got != uint64(id) {
		t.Fatalf("the double retire logged the segment as %v, want %d", got, uint64(id))
	}
	if got := logs.attr(rec, "op"); got != "retire" {
		t.Fatalf("the double retire logged the op as %v, want retire", got)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("after both retires, %v", err)
	}
}

// TestRecycleAfterRetireAddsNoSpare: recycling a segment a retire already
// unlinked must neither resurrect its cap as a spare nor change any counter:
// storage that left the footprint cannot come back through the recycle path.
func TestRecycleAfterRetireAddsNoSpare(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	var logs capturing
	s := NewSet(segCap, slog.New(&logs))
	old, fresh := core.SegmentID(1), core.SegmentID(2)

	for _, op := range []struct {
		name string
		fn   func() error
	}{
		{"AddSpare", func() error { return s.AddSpare(old) }},
		{"Take", func() error { return s.Take(old) }},
		{"Seal", func() error { return s.Seal(old) }},
	} {
		if err := op.fn(); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
	}
	if _, err := s.Retire(old); err != nil {
		t.Fatalf("the retire: %v", err)
	}
	before := s.Counters()

	err := s.Recycle(old, fresh)
	if !errors.Is(err, ErrNotInSet) {
		t.Fatalf("the recycle after the retire: %v, want %v", err, ErrNotInSet)
	}
	after := s.Counters()
	after.DoubleRetires--
	if after != before {
		t.Fatalf("the refused recycle changed more than the double-retire count: %v, still want %v", after, before)
	}
	if err := s.Take(fresh); !errors.Is(err, ErrNotInSet) {
		t.Fatalf("the recycle added the fresh id as a spare: Take returned %v, want %v", err, ErrNotInSet)
	}
	if len(logs.recs) != 1 {
		t.Fatalf("the ledger logged %d records, want the one Warn", len(logs.recs))
	}
	if rec := logs.recs[0]; logs.attr(rec, "op") != "recycle" {
		t.Fatalf("the refused recycle logged the op as %v, want recycle", logs.attr(rec, "op"))
	}
}

// TestRecycleKeepsFootprint: a recycle moves the segment's storage to its
// new id as a spare at the same accounted size, so the footprint does not
// move at all — only an unlink lowers it — while the spare pool grows by one
// and the recycled id is gone from both the set and the spares.
func TestRecycleKeepsFootprint(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	old, fresh := core.SegmentID(1), core.SegmentID(2)
	tagA, tagB := core.ShareTag(1), core.ShareTag(2)

	if err := s.AddSpare(old); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(old); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.AddRecord(old, tagA, 100); err != nil {
		t.Fatalf("AddRecord A: %v", err)
	}
	if err := s.AddRecord(old, tagB, 60); err != nil {
		t.Fatalf("AddRecord B: %v", err)
	}
	if err := s.Seal(old); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	before := s.Counters()

	if err := s.Recycle(old, fresh); err != nil {
		t.Fatalf("the recycle: %v", err)
	}
	after := s.Counters()
	if after.UsedBytes != before.UsedBytes {
		t.Fatalf("the recycle moved the footprint from %d to %d, want it unchanged", before.UsedBytes, after.UsedBytes)
	}
	if after.Segments != before.Segments-1 {
		t.Fatalf("the recycle left %d segments, want %d", after.Segments, before.Segments-1)
	}
	if after.Sealed != before.Sealed-1 {
		t.Fatalf("the recycle left %d sealed segments, want %d", after.Sealed, before.Sealed-1)
	}
	if after.Spares != before.Spares+1 {
		t.Fatalf("the recycle left %d spares, want %d", after.Spares, before.Spares+1)
	}
	if n := s.ShareBytes(tagA); n != 0 {
		t.Fatalf("the recycle kept %d record bytes of tag A, want them released", n)
	}
	if n := s.ShareBytes(tagB); n != 0 {
		t.Fatalf("the recycle kept %d record bytes of tag B, want them released", n)
	}
	if err := s.Take(fresh); err != nil {
		t.Fatalf("the fresh id is no spare: %v", err)
	}
	if err := s.Take(old); !errors.Is(err, ErrNotInSet) {
		t.Fatalf("the recycled id survived: Take returned %v, want %v", err, ErrNotInSet)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("after the recycle, %v", err)
	}
}

// TestFailedMakeReservationIsReturned: a spare whose making failed has its
// reservation returned through the set. The cleanup carries the leftover in
// with Take — the make reserved it, and no stream ever takes it as a segment —
// and once its unlink has happened, Retire returns the cap exactly once,
// uncounted and unlogged. A leftover whose unlink fails is carried in and
// ended instead, and stays accounted until a later retire takes it.
func TestFailedMakeReservationIsReturned(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	var logs capturing
	s := NewSet(segCap, slog.New(&logs))
	id := core.SegmentID(7)
	if err := s.AddSpare(id); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if c := s.Counters(); c.UsedBytes != segCap || c.Spares != 1 {
		t.Fatalf("the reservation holds the footprint at %d with %d spares, want the cap %d and one", c.UsedBytes, c.Spares, segCap)
	}

	// The make has failed: the leftover is carried in, ended — the state a
	// failure leaves, never sealed — and once its unlink has happened, the
	// retire returns the reservation.
	if err := s.Take(id); err != nil {
		t.Fatalf("Take of the failed make's leftover: %v", err)
	}
	if err := s.End(id); err != nil {
		t.Fatalf("End of the failed make's leftover: %v", err)
	}
	stuck := core.SegmentID(8)
	if err := s.AddSpare(stuck); err != nil {
		t.Fatalf("AddSpare of the leftover that cannot be unlinked: %v", err)
	}
	if err := s.Take(stuck); err != nil {
		t.Fatalf("Take of the stuck leftover: %v", err)
	}
	if err := s.End(stuck); err != nil {
		t.Fatalf("End of the stuck leftover: %v", err)
	}

	freed, err := s.Retire(id)
	if err != nil {
		t.Fatalf("the retire of the failed make's leftover: %v", err)
	}
	if freed != segCap {
		t.Fatalf("the retire freed %d, want the reserved cap %d", freed, segCap)
	}
	c := s.Counters()
	if c.UsedBytes != segCap {
		t.Fatalf("after the cleanup the footprint is %d, want the cap the stuck leftover still holds", c.UsedBytes)
	}
	if c.Segments != 1 || c.Spares != 0 {
		t.Fatalf("after the cleanup the ledger holds %d segments and %d spares, want one ended leftover and none", c.Segments, c.Spares)
	}
	if c.DoubleRetires != 0 {
		t.Fatalf("the cleanup was counted as %d double retires, want none: it is not the race the count watches for", c.DoubleRetires)
	}
	if len(logs.recs) != 0 {
		t.Fatalf("the cleanup logged %d records, want none", len(logs.recs))
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestAddSpareRefusals: an id the ledger already owns — in the set or among
// the spares — or an id no file can carry reserves nothing: the footprint
// answers with one cap per segment, never two, and never under an id that
// names no segment.
func TestAddSpareRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	if err := s.AddSpare(1); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	afterOne := s.Counters()

	if err := s.AddSpare(1); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("a second AddSpare of the same spare: %v, want %v", err, ErrDuplicateID)
	}
	if c := s.Counters(); c != afterOne {
		t.Fatalf("the refused duplicate raised the footprint: %v, still want %v", c, afterOne)
	}
	if err := s.Take(1); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.AddSpare(1); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("an AddSpare of a set segment's id: %v, want %v", err, ErrDuplicateID)
	}
	if c := s.Counters(); c.UsedBytes != segCap || c.Spares != 0 || c.Segments != 1 {
		t.Fatalf("the refused duplicate left the ledger at %v, want one cap, no spares, one segment", c)
	}

	if err := s.AddSpare(0); !errors.Is(err, ErrBadID) {
		t.Fatalf("an AddSpare of the id that names no segment: %v, want %v", err, ErrBadID)
	}
	if c := s.Counters(); c.UsedBytes != segCap || c.Spares != 0 {
		t.Fatalf("the refused id raised the footprint to %d with %d spares, want it still one cap, none", c.UsedBytes, c.Spares)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestRetireOfASpareIsRefused: the retirement paths select segments, never
// spares, so naming one is a caller bug the ledger refuses without counting:
// like a retire of an Active segment, it is not the race the double-retire
// count watches for, and the spare stays held for the pool either way.
func TestRetireOfASpareIsRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	var logs capturing
	s := NewSet(segCap, slog.New(&logs))
	id := core.SegmentID(3)
	if err := s.AddSpare(id); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	before := s.Counters()

	for name, refuse := range map[string]func() error{
		"retire": func() error {
			_, err := s.Retire(id)
			return err
		},
		"recycle": func() error { return s.Recycle(id, 9) },
	} {
		if err := refuse(); !errors.Is(err, ErrWrongState) {
			t.Fatalf("the %s of a spare: %v, want %v", name, err, ErrWrongState)
		}
	}
	if after := s.Counters(); after != before {
		t.Fatalf("the refused retire and recycle of a spare changed the counters: %v, want %v", after, before)
	}
	if err := s.Take(id); err != nil {
		t.Fatalf("the spare did not survive: Take returned %v", err)
	}
	if len(logs.recs) != 0 {
		t.Fatalf("the ledger logged %d records, want none: this is not a double retire", len(logs.recs))
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestAddRecordOfNoBytesIsRefused: a record of no bytes accounts nothing, and
// a negative one would take bytes off a share; both leave the share's totals
// and every counter exactly as they were.
func TestAddRecordOfNoBytesIsRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	id := core.SegmentID(1)
	tag := core.ShareTag(2)
	if err := s.AddSpare(id); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(id); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.AddRecord(id, tag, 100); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	before := s.Counters()

	for _, n := range []int64{0, -1, -100} {
		if err := s.AddRecord(id, tag, n); !errors.Is(err, ErrBadSize) {
			t.Fatalf("AddRecord of %d bytes: %v, want %v", n, err, ErrBadSize)
		}
	}
	if after := s.Counters(); after != before {
		t.Fatalf("the refused records changed the counters: %v, still want %v", after, before)
	}
	if n := s.ShareBytes(tag); n != 100 {
		t.Fatalf("the refused records moved the share's bytes to %d, want 100", n)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestRecycleRefusals: a failed recycle — the old segment sealed early or
// not sealed at all, or the fresh id already owned — leaves every counter and
// every map exactly as it was: every refusal is checked before anything
// changes.
func TestRecycleRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	const half = segCap / 2

	// refuse returns the error class the case expects. None of these refusals
	// may count a double retire: the old segment was found in the set, only
	// in a state or at a size a recycle refuses.
	refuse := func(setup func(s *Set) error, old, fresh core.SegmentID) error {
		s := NewSet(segCap, nil) // a fresh set, so no case sees another's ids
		if err := setup(s); err != nil {
			t.Fatalf("setup: %v", err)
		}
		before := s.Counters()
		err := s.Recycle(old, fresh)
		if err == nil {
			t.Fatalf("recycling %d as %d was not refused", old, fresh)
		}
		if after := s.Counters(); after != before {
			t.Fatalf("the refused recycle of %d changed the counters: %v, still want %v", old, after, before)
		}
		if err := s.Check(); err != nil {
			t.Fatalf("after the refused recycle of %d, %v", old, err)
		}
		return err
	}

	// The make of a segment that a recycle can take: one spare, in the set,
	// sealed, holding nothing.
	spent := func(state func(s *Set, id core.SegmentID) error) func(s *Set) error {
		return func(s *Set) error {
			if err := s.AddSpare(1); err != nil {
				return err
			}
			if err := s.Take(1); err != nil {
				return err
			}
			return state(s, 1)
		}
	}
	eventaken := spent(func(s *Set, id core.SegmentID) error {
		if err := s.Seal(id); err != nil {
			return err
		}
		return s.Shrink(id, half)
	})
	sealed := spent(func(s *Set, id core.SegmentID) error { return s.Seal(id) })
	ended := spent(func(s *Set, _ core.SegmentID) error { return s.End(1) })
	active := spent(func(*Set, core.SegmentID) error { return nil })
	sealedWithASpare := func(s *Set) error {
		if err := s.AddSpare(1); err != nil {
			return err
		}
		if err := s.Take(1); err != nil {
			return err
		}
		if err := s.Seal(1); err != nil {
			return err
		}
		return s.AddSpare(2)
	}
	sealedWithASegment := func(s *Set) error {
		if err := s.AddSpare(1); err != nil {
			return err
		}
		if err := s.AddSpare(2); err != nil {
			return err
		}
		if err := s.Take(1); err != nil {
			return err
		}
		if err := s.Take(2); err != nil {
			return err
		}
		return s.Seal(1)
	}

	for _, c := range []struct {
		name  string
		want  error
		setup func(s *Set) error
		old   core.SegmentID
		fresh core.SegmentID
	}{
		{"a segment sealed early was truncated below its cap", ErrBadSize, eventaken, 1, 9},
		{"a segment ended by a failed append is never recycled", ErrWrongState, ended, 1, 9},
		{"a segment a stream still appends to is never recycled", ErrWrongState, active, 1, 9},
		{"the fresh id is already a spare", ErrDuplicateID, sealedWithASpare, 1, 2},
		{"the fresh id is already a segment of the set", ErrDuplicateID, sealedWithASegment, 1, 2},
		{"the fresh id is the recycled segment itself", ErrDuplicateID, sealed, 1, 1},
	} {
		if err := refuse(c.setup, c.old, c.fresh); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// TestReleaseAfterRetireChangesNothing: a claim lives on the segment's entry,
// so retiring the segment drops it, a later Release finds nothing and changes
// nothing, and a later Claim loses. A claim on an Active segment is refused,
// and a held claim is not handed out twice.
func TestReleaseAfterRetireChangesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	retired := core.SegmentID(1)
	if err := s.AddSpare(retired); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(retired); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.Seal(retired); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !s.Claim(retired) {
		t.Fatal("Claim on a sealed segment was refused, want it held")
	}
	if _, err := s.Retire(retired); err != nil {
		t.Fatalf("the retire: %v", err)
	}
	around := s.Counters()

	s.Release(retired) // the claim is gone with the entry: a no-op
	after := s.Counters()
	if after != around {
		t.Fatalf("Release after the retire changed the counters: %v, want %v", after, around)
	}
	if s.Claim(retired) {
		t.Fatal("Claim on a retired segment was held, want it lost")
	}
	s.Release(retired) // still nothing to release
	if s.Counters() != around {
		t.Fatalf("the second Release changed the counters, want %v", around)
	}

	// A claim is held only on a segment no stream can still append to.
	stillActive := core.SegmentID(2)
	if err := s.AddSpare(stillActive); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(stillActive); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if s.Claim(stillActive) {
		t.Fatal("Claim on an Active segment was held, want it refused")
	}
	if err := s.Seal(stillActive); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !s.Claim(stillActive) {
		t.Fatal("the first Claim on a sealed segment was refused, want it held")
	}
	if s.Claim(stillActive) {
		t.Fatal("the second Claim on one segment was held too, want it refused")
	}
	s.Release(stillActive)
	if !s.Claim(stillActive) {
		t.Fatal("Claim after Release was refused, want the claim back")
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestShrinkAtSealAndEndedKeepsCap: a segment shrunk at seal is accounted at
// its new size and retires exactly that, while a segment ended by a failure
// keeps its cap until it is unlinked. A shrink that would grow the segment
// or empty it is refused.
func TestShrinkAtSealAndEndedKeepsCap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	sealedEarly := core.SegmentID(1)
	if err := s.AddSpare(sealedEarly); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(sealedEarly); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.Seal(sealedEarly); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	const tail = 512
	if err := s.Shrink(sealedEarly, tail); err != nil {
		t.Fatalf("Shrink at seal: %v", err)
	}
	if c := s.Counters(); c.UsedBytes != tail {
		t.Fatalf("a segment shrunk at seal is accounted at %d, want %d", c.UsedBytes, tail)
	}
	freed, err := s.Retire(sealedEarly)
	if err != nil {
		t.Fatalf("the retire: %v", err)
	}
	if freed != tail {
		t.Fatalf("the retire freed %d, want the shrunk size %d", freed, tail)
	}

	// An ended segment keeps its cap until it is unlinked: the failure
	// strands the storage, not the accounting.
	ended := core.SegmentID(2)
	if err := s.AddSpare(ended); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(ended); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.End(ended); err != nil {
		t.Fatalf("End: %v", err)
	}
	if c := s.Counters(); c.UsedBytes != segCap {
		t.Fatalf("End moved the footprint to %d, want the cap %d kept", c.UsedBytes, segCap)
	}
	// A shrink to the size the segment is already accounted at is the one
	// size above ErrBadSize's floor: it takes no tail, so it lowers nothing.
	if err := s.Shrink(ended, segCap); err != nil {
		t.Fatalf("a shrink to the current size: %v, want it accepted as the no-op it is", err)
	}
	if c := s.Counters(); c.UsedBytes != segCap {
		t.Fatalf("the no-op shrink moved the footprint to %d, want the cap %d kept", c.UsedBytes, segCap)
	}
	freed, err = s.Retire(ended)
	if err != nil {
		t.Fatalf("the retire of the ended segment: %v", err)
	}
	if freed != segCap {
		t.Fatalf("the ended segment retired at %d, want its kept cap %d", freed, segCap)
	}

	// A torn-tail cut of an ended segment is the one shrink that applies to
	// it: the cut size is what the retire returns afterwards.
	torn := core.SegmentID(3)
	if err := s.AddSpare(torn); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(torn); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.End(torn); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := s.Shrink(torn, tail); err != nil {
		t.Fatalf("Shrink of an ended segment: %v", err)
	}
	freed, err = s.Retire(torn)
	if err != nil {
		t.Fatalf("the retire of the torn ended segment: %v", err)
	}
	if freed != tail {
		t.Fatalf("the torn ended segment retired at %d, want its cut size %d", freed, tail)
	}

	// The size bounds, on one sealed segment: a shrink may keep the size or
	// lower it, never grow it and never empty it.
	growing := core.SegmentID(4)
	if err := s.AddSpare(growing); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(growing); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.Seal(growing); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	const kept = 256
	if err := s.Shrink(growing, kept); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	before := s.Counters()
	if err := s.Shrink(growing, kept+1); !errors.Is(err, ErrBadSize) {
		t.Fatalf("a growing shrink: %v, want %v", err, ErrBadSize)
	}
	if err := s.Shrink(growing, kept); err != nil {
		t.Fatalf("a shrink to the current size: %v, want the no-op it is", err)
	}
	if err := s.Shrink(growing, segCap); !errors.Is(err, ErrBadSize) {
		t.Fatalf("a shrink to the cap: %v, want %v: it grows the accounted size", err, ErrBadSize)
	}
	if err := s.Shrink(growing, 0); !errors.Is(err, ErrBadSize) {
		t.Fatalf("a shrink to nothing: %v, want %v", err, ErrBadSize)
	}
	if err := s.Shrink(growing, -1); !errors.Is(err, ErrBadSize) {
		t.Fatalf("a negative shrink: %v, want %v", err, ErrBadSize)
	}
	if after := s.Counters(); after != before {
		t.Fatalf("the refused shrinks changed the counters: %v, want %v", after, before)
	}
}

// TestShrinkRefusesAnActiveSegment: a segment is accounted at its cap from
// its takeover to its seal or its end, so the two cuts that shorten it — a
// seal's truncation and a torn-tail cut of an ended segment — both land after
// the stream left it. A shrink while a stream still appends would sit the
// footprint below the storage the file allocates, a ledger no size check of
// the entries' own fields can see, so it is refused as a wrong state and the
// segment keeps everything: its footprint, its records and its future.
func TestShrinkRefusesAnActiveSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	id := core.SegmentID(1)
	if err := s.AddSpare(id); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(id); err != nil {
		t.Fatalf("Take: %v", err)
	}
	before := s.Counters()

	if err := s.Shrink(id, half(segCap)); !errors.Is(err, ErrWrongState) {
		t.Fatalf("a shrink of an Active segment: %v, want %v", err, ErrWrongState)
	}
	if err := s.Shrink(id, segCap); !errors.Is(err, ErrWrongState) {
		t.Fatalf("a shrink of an Active segment to the size it is accounted at: %v, want %v: no state below Sealed serves a shrink", err, ErrWrongState)
	}
	if after := s.Counters(); after != before {
		t.Fatalf("the refused shrinks changed the counters: %v, want %v", after, before)
	}
	if n := s.ShareBytes(1); n > 0 {
		t.Fatalf("the refused shrinks released record bytes: %d", n)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}

	// The refusal is the state's, not the id's: once the segment is ended,
	// the torn-tail cut the refused shrink stood in for is served.
	if err := s.End(id); err != nil {
		t.Fatalf("End after the refused shrinks: %v", err)
	}
	if err := s.Shrink(id, half(segCap)); err != nil {
		t.Fatalf("the torn-tail cut of the ended segment: %v", err)
	}
	if c := s.Counters(); c.UsedBytes != half(segCap) {
		t.Fatalf("the ended segment is accounted at %d, want the cut size %d", c.UsedBytes, half(segCap))
	}
}

// half returns half of n, for a cap's middle.
func half(n int64) int64 { return n / 2 }

// TestRecordBytesLeaveWithTheirSegment: every record's bytes are accounted to
// the share whose tag the record carries, and each segment releases only its
// own bytes from the share's totals, so a tag that reaches zero leaves the
// ledger's map. The footprint never moves with a record: the bytes are inside
// storage the segment already accounts for.
func TestRecordBytesLeaveWithTheirSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	tagA, tagB, tagC := core.ShareTag(1), core.ShareTag(2), core.ShareTag(3)
	one, two, three := core.SegmentID(1), core.SegmentID(2), core.SegmentID(3)

	seed := func(id core.SegmentID) {
		if err := s.AddSpare(id); err != nil {
			t.Fatalf("AddSpare %d: %v", id, err)
		}
		if err := s.Take(id); err != nil {
			t.Fatalf("Take %d: %v", id, err)
		}
	}
	for _, id := range []core.SegmentID{one, two, three} {
		seed(id)
	}

	footprint := s.Counters().UsedBytes
	if err := s.AddRecord(one, tagA, 100); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if c := s.Counters(); c.UsedBytes != footprint {
		t.Fatalf("a record moved the footprint to %d, want %d: the segment already accounts for its storage", c.UsedBytes, footprint)
	}
	for _, r := range []struct {
		id  core.SegmentID
		tag core.ShareTag
		n   int64
	}{
		{one, tagB, 60},
		{two, tagA, 40},
		{two, tagC, 10},
	} {
		if err := s.AddRecord(r.id, r.tag, r.n); err != nil {
			t.Fatalf("AddRecord(%d, %d): %v", r.id, r.tag, err)
		}
	}

	if err := s.Seal(one); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := s.Seal(two); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	freed, err := s.Retire(one)
	if err != nil {
		t.Fatalf("the retire of segment one: %v", err)
	}
	if freed != segCap {
		t.Fatalf("the retire of segment one freed %d, want %d", freed, segCap)
	}
	if n := s.ShareBytes(tagA); n != 40 {
		t.Fatalf("tag A holds %d bytes, want the other segment's 40", n)
	}
	if n := s.ShareBytes(tagB); n != 0 {
		t.Fatalf("tag B holds %d bytes, want 0: its only records left", n)
	}
	if n := s.ShareBytes(tagC); n != 10 {
		t.Fatalf("tag C holds %d bytes, want 10", n)
	}
	if len(s.tags) != 2 {
		t.Errorf("the ledger keeps %d tags, want the two with bytes still held", len(s.tags))
	}

	if _, err := s.Retire(two); err != nil {
		t.Fatalf("the retire of segment two: %v", err)
	}
	if len(s.tags) != 0 {
		t.Errorf("the ledger keeps %d tags, want none: every tag reached zero", len(s.tags))
	}

	if err := s.Seal(three); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := s.AddRecord(three, tagA, 5); !errors.Is(err, ErrWrongState) {
		t.Errorf("a record into a sealed segment: %v, want %v", err, ErrWrongState)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestCheckReportsMismatch: the check compares the accounted footprint against
// a walk of the set's sizes plus every spare at the cap — spares included, or
// it would report a set holding spares as broken — and reports a mismatch it
// can catch only while the two figures really differ, naming both.
func TestCheckReportsMismatch(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	for _, id := range []core.SegmentID{1, 2} {
		if err := s.AddSpare(id); err != nil {
			t.Fatalf("AddSpare: %v", err)
		}
	}
	if err := s.Take(1); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.AddRecord(1, 4, 100); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if err := s.Seal(1); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// One spare remains beside the set, so the check must count it at the cap.
	if err := s.Check(); err != nil {
		t.Fatalf("the check of a consistent set holding a spare: %v", err)
	}

	const footprint = 2 * segCap // the two caps: the set's segment and the spare
	// A retire that forgot to subtract: the counters hold a figure the set
	// cannot add up to.
	s.mu.Lock()
	s.c.UsedBytes -= segCap
	s.mu.Unlock()
	err := s.Check()
	if err == nil {
		t.Fatal("a skipped subtract was not reported")
	}
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("the mismatch was reported as %v, want it to wrap %v", err, ErrMismatch)
	}
	msg := err.Error()
	for _, figure := range []int64{segCap, footprint} {
		if !strings.Contains(msg, strconv.FormatInt(figure, 10)) {
			t.Fatalf("the mismatch %q does not name the figure %d", msg, figure)
		}
	}

	// Each maintained count is checked against the walk too: a counter that
	// drifted while the footprint stayed true is a broken ledger the same
	// way. Each corruption is made with every other figure true, so only
	// its own clause can be the one that reports it.
	for _, corrupt := range []func(c *Counters){
		func(c *Counters) { c.Segments++ },
		func(c *Counters) { c.Sealed++ },
		func(c *Counters) { c.Spares++ },
	} {
		s.mu.Lock()
		s.c.Segments, s.c.Sealed, s.c.Spares = 1, 1, 1 // the true counts
		s.c.UsedBytes = footprint
		corrupt(&s.c)
		s.mu.Unlock()
		err := s.Check()
		if err == nil {
			t.Fatal("a drifted count was not reported")
		}
		if !errors.Is(err, ErrMismatch) {
			t.Fatalf("the drifted count was reported as %v, want it to wrap %v", err, ErrMismatch)
		}
		if got := err.Error(); !strings.Contains(got, "vs") {
			t.Fatalf("the drifted count %q does not name both figures", got)
		}
	}

	// Every figure back to the truth.
	restore := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.c.Segments, s.c.Sealed, s.c.Spares = 1, 1, 1
		s.c.UsedBytes = footprint
		s.tags[4] = 100
		delete(s.tags, 9)
	}

	// The per-tag totals are checked against the entries' own tags too: a
	// release that dropped or doubled a share's bytes is a broken ledger the
	// check, not only the fuzz oracle, has to see.
	restore()
	s.mu.Lock()
	s.tags[4] += 1 // one byte more than any entry holds
	s.mu.Unlock()
	if err := s.Check(); err == nil {
		t.Fatal("a share total that no entry makes up was not reported")
	} else if !errors.Is(err, ErrMismatch) {
		t.Fatalf("the share total was reported as %v, want it to wrap %v", err, ErrMismatch)
	}

	// A key the entries do not hold at all — a negative total a release
	// leaves behind rather than deletes — is its own mismatch: the key sets
	// differ, whatever the values add up to.
	restore()
	s.mu.Lock()
	s.tags[9] = -5 // a key no entry makes up
	s.mu.Unlock()
	if err := s.Check(); err == nil {
		t.Fatal("a share total with no entry behind it was not reported")
	} else if !errors.Is(err, ErrMismatch) {
		t.Fatalf("the entryless share total was reported as %v, want it to wrap %v", err, ErrMismatch)
	}

	// Everything true again: a consistent set holding a spare and a record
	// checks clean, or the corruption cases would pass their own assertions.
	restore()
	if err := s.Check(); err != nil {
		t.Fatalf("the check of the consistent set after the corruptions: %v", err)
	}
}

// TestRetireActiveRefused: a segment a stream still appends to is refused
// without a count: it stayed in the set with everything it held, and the
// refusal is a different bug from the double retire the count watches for.
func TestRetireActiveRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	id := core.SegmentID(1)
	if err := s.AddSpare(id); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(id); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.AddRecord(id, 1, 32); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	before := s.Counters()

	freed, err := s.Retire(id)
	if !errors.Is(err, ErrWrongState) {
		t.Fatalf("the retire of an Active segment: %v, want %v", err, ErrWrongState)
	}
	if freed != 0 {
		t.Fatalf("the refused retire freed %d, want 0", freed)
	}
	after := s.Counters()
	if after != before {
		t.Fatalf("the refused retire changed the counters: %v, want %v", after, before)
	}
	if n := s.ShareBytes(1); n != 32 {
		t.Fatalf("the refused retire released the segment's record bytes to %d", n)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
	// The segment is untouched: sealing it and retiring it still works.
	if err := s.Seal(id); err != nil {
		t.Fatalf("Seal after the refused retire: %v", err)
	}
	if _, err := s.Retire(id); err != nil {
		t.Fatalf("the retire after sealing: %v", err)
	}
}

// TestCountersAllocateNothing: a counters read and the accounting of a
// record's bytes run on the path of every operation of the writing side, so
// neither may allocate: a read that walks, or an allocation per record,
// would allocate forever.
func TestCountersAllocateNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	id := core.SegmentID(1)
	tag := core.ShareTag(1)
	if err := s.AddSpare(id); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(id); err != nil {
		t.Fatalf("Take: %v", err)
	}
	// The map of the (segment, tag) pair exists from here on, so every
	// measured call only finds and updates.
	if err := s.AddRecord(id, tag, 64); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}

	var c Counters
	allocs := testing.AllocsPerRun(100, func() { c = s.Counters() })
	_ = c
	if allocs != 0 {
		t.Fatalf("a counters read allocated %v times, want 0", allocs)
	}

	var err error
	allocs = testing.AllocsPerRun(100, func() { err = s.AddRecord(id, tag, 8) })
	if err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if allocs != 0 {
		t.Fatalf("the accounting of a record allocated %v times, want 0", allocs)
	}
}

// TestNewSetPanicsOnABadCap: the cap is the size every spare is accounted at
// and the ceiling of every shrink, so a non-positive one is a caller bug the
// ledger refuses at construction.
func TestNewSetPanicsOnABadCap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, segCap := range []int64{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("a cap of %d did not panic, want a caller bug caught at construction", segCap)
				}
			}()
			_ = NewSet(segCap, nil)
		}()
	}
}

// TestStateString: the state's name is what a log line or an inspection
// prints.
func TestStateString(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, c := range []struct {
		s    State
		want string
	}{
		{Active, "active"},
		{Sealed, "sealed"},
		{Ended, "ended"},
		{State(0), "unknown"},
		{State(200), "unknown"},
	} {
		if got := c.s.String(); got != c.want {
			t.Errorf("State(%d) prints as %q, want %q", uint8(c.s), got, c.want)
		}
	}
}
