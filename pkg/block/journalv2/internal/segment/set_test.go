package segment

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// noMax is the journal maximum a spare's reservation is checked against
// where the maximum is not under test: a limit no footprint adds up to.
const noMax = math.MaxInt64

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
		{"AddSpare", func() error { return s.AddSpare(id, noMax) }},
		{"Take", func() error { return s.Take(id) }},
		{"Seal", func() error { return s.Seal(id, true) }},
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

// TestAddSpareRefusedOneShortOfTheMaximum: a spare's cap is reserved against
// the journal's maximum atomically with the ledger's UsedBytes, so a maximum
// one byte short of the footprint plus the cap refuses the spare with a
// NoSpaceError naming the journal limit, exactly one byte missing and the
// bytes unit — catching an off-by-one and a wrong unit — while changing no
// counter, no spare and the check's verdict: the refusal is not a double
// retire either.
func TestAddSpareRefusedOneShortOfTheMaximum(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	first := core.SegmentID(1)
	if err := s.AddSpare(first, noMax); err != nil {
		t.Fatalf("the first spare: %v", err)
	}
	before := s.Counters()
	if err := s.Check(); err != nil {
		t.Fatalf("before the refusal, %v", err)
	}

	err := s.AddSpare(core.SegmentID(2), segCap+segCap-1)
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("the refusal: %v, want a *core.NoSpaceError", err)
	}
	if ns.Limit != core.LimitJournal {
		t.Fatalf("the refusal named the %s limit, want %s", ns.Limit, core.LimitJournal)
	}
	if ns.Missing != 1 {
		t.Fatalf("the refusal said %d missing, want 1", ns.Missing)
	}
	if ns.Unit != core.UnitBytes {
		t.Fatalf("the refusal counted %s, want %s", ns.Unit, core.UnitBytes)
	}

	after := s.Counters()
	if after != before {
		t.Fatalf("the refusal changed the counters: %v, still want %v", after, before)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("after the refusal, %v", err)
	}
}

// TestAddSpareAcceptedAtExactlyTheMaximum: a maximum that holds the footprint
// plus exactly one more cap takes the spare: the bound is on what the journal
// holds, not on what it holds minus one.
func TestAddSpareAcceptedAtExactlyTheMaximum(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	if err := s.AddSpare(core.SegmentID(1), noMax); err != nil {
		t.Fatalf("the first spare: %v", err)
	}
	if err := s.AddSpare(core.SegmentID(2), 2*segCap); err != nil {
		t.Fatalf("the spare at exactly the maximum: %v", err)
	}
	c := s.Counters()
	if c.UsedBytes != 2*segCap || c.Spares != 2 {
		t.Fatalf("after the spare at the maximum, the footprint is %d with %d spares, want %d and 2",
			c.UsedBytes, c.Spares, 2*segCap)
	}
	if err := s.AddSpare(core.SegmentID(3), 2*segCap); err == nil {
		t.Fatal("the spare past the full maximum was taken, want a refusal")
	} else if !errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("the spare past the full maximum: %v, want a refusal matching %v", err, core.ErrNoSpace)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("after the maximum, %v", err)
	}
}

// TestAddSpareRefusedBelowItsOwnCap: a maximum below the cap itself refuses
// every spare without overflows: the overflow-safe form of the check must
// read it as a refusal, not wrap into an acceptance.
func TestAddSpareRefusedBelowItsOwnCap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	for _, max := range []int64{0, -1, segCap - 1} {
		err := s.AddSpare(core.SegmentID(1), max)
		if !errors.Is(err, core.ErrNoSpace) {
			t.Fatalf("AddSpare at the maximum %d: %v, want a refusal matching %v", max, err, core.ErrNoSpace)
		}
	}
	if err := s.Check(); err != nil {
		t.Fatalf("after the refusals, %v", err)
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
		{"AddSpare", func() error { return s.AddSpare(old, noMax) }},
		{"Take", func() error { return s.Take(old) }},
		{"Seal", func() error { return s.Seal(old, true) }},
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
	rec := logs.recs[0]
	if rec.Level != slog.LevelWarn {
		t.Fatalf("the refused recycle logged at %v, want Warn", rec.Level)
	}
	if rec.Message != absentMessage {
		t.Fatalf("the refused recycle logged %q, want %q", rec.Message, absentMessage)
	}
	if got := logs.attr(rec, "segment"); got != uint64(old) {
		t.Fatalf("the refused recycle logged the segment as %v, want the recycled id %d", got, uint64(old))
	}
	if got := logs.attr(rec, "op"); got != "recycle" {
		t.Fatalf("the refused recycle logged the op as %v, want recycle", got)
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

	if err := s.AddSpare(old, noMax); err != nil {
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
	if err := s.Seal(old, true); err != nil {
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
// reservation returned in one step, once its unlink has happened: DropSpare
// takes the cap back and the spare out, in the single call a failed make
// owes. The leftover that cannot be unlinked stays in the set, ended, until
// a later retire takes it — a spare cannot go back to being one.
func TestFailedMakeReservationIsReturned(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	var logs capturing
	s := NewSet(segCap, slog.New(&logs))
	id := core.SegmentID(7)
	if err := s.AddSpare(id, noMax); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if c := s.Counters(); c.UsedBytes != segCap || c.Spares != 1 {
		t.Fatalf("the reservation holds the footprint at %d with %d spares, want the cap %d and one", c.UsedBytes, c.Spares, segCap)
	}

	// The make has failed and its unlink has happened: the drop returns the
	// reservation exactly once, uncounted and unlogged — Segments never held
	// a segment no stream ever had.
	freed, err := s.DropSpare(id)
	if err != nil {
		t.Fatalf("the drop of the failed make's spare: %v", err)
	}
	if freed != segCap {
		t.Fatalf("the drop freed %d, want the reserved cap %d", freed, segCap)
	}
	c := s.Counters()
	if c.UsedBytes != 0 || c.Spares != 0 || c.Segments != 0 {
		t.Fatalf("after the drop the ledger holds %d bytes, %d spares and %d segments, want all three empty", c.UsedBytes, c.Spares, c.Segments)
	}
	if c.DoubleRetires != 0 {
		t.Fatalf("the drop was counted as %d double retires, want none: it found its spare", c.DoubleRetires)
	}
	if len(logs.recs) != 0 {
		t.Fatalf("the drop logged %d records, want none", len(logs.recs))
	}

	// A leftover whose unlink fails cannot be dropped: it is carried into
	// the set with Take and ended — the state a failure leaves, never sealed
	// — and stays accounted until a later retire takes it.
	stuck := core.SegmentID(8)
	if err := s.AddSpare(stuck, noMax); err != nil {
		t.Fatalf("AddSpare of the leftover that cannot be unlinked: %v", err)
	}
	if err := s.Take(stuck); err != nil {
		t.Fatalf("Take of the stuck leftover: %v", err)
	}
	if err := s.End(stuck); err != nil {
		t.Fatalf("End of the stuck leftover: %v", err)
	}
	if _, err := s.DropSpare(stuck); !errors.Is(err, ErrWrongState) {
		t.Fatalf("the drop of a segment of the set: %v, want %v", err, ErrWrongState)
	}
	if c := s.Counters(); c.UsedBytes != segCap || c.Segments != 1 {
		t.Fatalf("after the refused drop the ledger holds %d bytes and %d segments, want the cap and the one stuck leftover", c.UsedBytes, c.Segments)
	}
	freed, err = s.Retire(stuck)
	if err != nil {
		t.Fatalf("the retire of the stuck leftover: %v", err)
	}
	if freed != segCap {
		t.Fatalf("the stuck leftover retired at %d, want its cap %d", freed, segCap)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestDropSpareRefusals: a drop that finds no spare — because the id names a
// segment of the set, or because another drop already took the spare out
// — leaves everything as it was. The absent id is the race the double-retire
// count watches for, so it counts and logs at Warn exactly as Retire does;
// the segment is a caller bug, refused uncounted for the same reason Retire
// refuses a spare.
func TestDropSpareRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	var logs capturing
	s := NewSet(segCap, slog.New(&logs))
	spare, seg, absent := core.SegmentID(1), core.SegmentID(2), core.SegmentID(3)
	for _, id := range []core.SegmentID{spare, seg} {
		if err := s.AddSpare(id, noMax); err != nil {
			t.Fatalf("AddSpare %d: %v", id, err)
		}
	}
	if err := s.Take(seg); err != nil {
		t.Fatalf("Take: %v", err)
	}
	before := s.Counters()

	if freed, err := s.DropSpare(seg); !errors.Is(err, ErrWrongState) || freed != 0 {
		t.Fatalf("the drop of a set segment: freed %d, %v, want 0 and %v", freed, err, ErrWrongState)
	}
	if after := s.Counters(); after != before {
		t.Fatalf("the refused drop of a segment changed the counters: %v, want %v", after, before)
	}

	// The absent id: the second drop of a spare two paths both tried to
	// unlink. It frees nothing, counts one double retire and logs at Warn.
	freed, err := s.DropSpare(absent)
	if !errors.Is(err, ErrNotInSet) {
		t.Fatalf("the drop of an absent id: %v, want %v", err, ErrNotInSet)
	}
	if freed != 0 {
		t.Fatalf("the drop of an absent id freed %d, want 0", freed)
	}
	after := s.Counters()
	after.DoubleRetires--
	if after != before {
		t.Fatalf("the drop of an absent id changed more than the double-retire count: %v, want %v", after, before)
	}
	if len(logs.recs) != 1 {
		t.Fatalf("the ledger logged %d records, want the one Warn", len(logs.recs))
	}
	rec := logs.recs[0]
	if rec.Level != slog.LevelWarn {
		t.Fatalf("the absent drop logged at %v, want Warn", rec.Level)
	}
	if got := logs.attr(rec, "segment"); got != uint64(absent) {
		t.Fatalf("the absent drop logged the segment as %v, want %d", got, uint64(absent))
	}
	if got := logs.attr(rec, "op"); got != "drop" {
		t.Fatalf("the absent drop logged the op as %v, want drop", got)
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
	if err := s.AddSpare(id, noMax); err != nil {
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

// TestAnEarlySealIsNeverRecycled: recycled storage is a full spare's — the
// seal said full, so nothing was truncated out of the file. A segment sealed
// early is never recycled, whichever of its two shapes it stands in: the
// truncation that has not landed yet, so the file still sits at its cap, and
// the truncation whose sync failed, which leaves the file at its cap for good
// (RFC 28 §2.5). The second shape is the one the size cannot tell from a full
// seal — hence the seal's own word for itself. Each refusal changes nothing.
func TestAnEarlySealIsNeverRecycled(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	id, fresh := core.SegmentID(1), core.SegmentID(2)
	tag := core.ShareTag(2)
	if err := s.AddSpare(id, noMax); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(id); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.AddRecord(id, tag, 100); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}

	// The seal named early: the truncation it stands for has not landed yet,
	// and the file still holds the whole cap.
	if err := s.Seal(id, false); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	before := s.Counters()
	if err := s.Recycle(id, fresh); !errors.Is(err, ErrBadSize) {
		t.Fatalf("the recycle of an early-sealed segment at its cap: %v, want %v", err, ErrBadSize)
	}
	if after := s.Counters(); after != before {
		t.Fatalf("the refused recycle changed the counters: %v, still want %v", after, before)
	}
	if err := s.Take(fresh); !errors.Is(err, ErrNotInSet) {
		t.Fatalf("the refused recycle minted the fresh id anyway: Take returned %v, want %v", err, ErrNotInSet)
	}

	// The truncation lands: the file keeps its records and gives up its tail.
	// Still not storage for a full spare, whatever the size now says.
	if err := s.Shrink(id, 512); err != nil {
		t.Fatalf("the truncation after the early seal: %v", err)
	}
	before = s.Counters()
	if err := s.Recycle(id, fresh); !errors.Is(err, ErrBadSize) {
		t.Fatalf("the recycle of an early-sealed and truncated segment: %v, want %v", err, ErrBadSize)
	}
	if after := s.Counters(); after != before {
		t.Fatalf("the second refused recycle changed the counters: %v, still want %v", after, before)
	}

	// The reservation leaves by an unlink, the one road it has.
	freed, err := s.Retire(id)
	if err != nil {
		t.Fatalf("the retire of the early-sealed segment: %v", err)
	}
	if freed != 512 {
		t.Fatalf("the retire freed %d, want the truncated size 512", freed)
	}
	if n := s.ShareBytes(tag); n != 0 {
		t.Fatalf("the retire left %d record bytes behind, want them released", n)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestAFullSealKeepsItsCap: a full seal closed the segment at its cap with
// nothing truncated, so no cut of it exists and Shrink refuses it. The seal
// named early is the one that serves its truncation: the cut is the
// truncation it stands for. Both seals leave the counters whole when refused.
func TestAFullSealKeepsItsCap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	full, early := core.SegmentID(1), core.SegmentID(2)
	for _, id := range []core.SegmentID{full, early} {
		if err := s.AddSpare(id, noMax); err != nil {
			t.Fatalf("AddSpare %d: %v", id, err)
		}
		if err := s.Take(id); err != nil {
			t.Fatalf("Take %d: %v", id, err)
		}
	}
	if err := s.Seal(full, true); err != nil {
		t.Fatalf("the full seal: %v", err)
	}
	if err := s.Seal(early, false); err != nil {
		t.Fatalf("the early seal: %v", err)
	}
	before := s.Counters()

	if err := s.Shrink(full, 512); !errors.Is(err, ErrWrongState) {
		t.Fatalf("a cut of a fully sealed segment: %v, want %v: its truncation never existed", err, ErrWrongState)
	}
	if after := s.Counters(); after != before {
		t.Fatalf("the refused cut changed the counters: %v, still want %v", after, before)
	}
	// The early seal's truncation is served, and its retire returns what the
	// cut left.
	if err := s.Shrink(early, 512); err != nil {
		t.Fatalf("the truncation after the early seal: %v", err)
	}
	if c := s.Counters(); c.UsedBytes != segCap+512 {
		t.Fatalf("after the cut the footprint is %d, want the full seal's cap plus the truncated 512", c.UsedBytes)
	}
	freed, err := s.Retire(early)
	if err != nil {
		t.Fatalf("the retire of the truncated segment: %v", err)
	}
	if freed != 512 {
		t.Fatalf("the truncated segment retired at %d, want 512", freed)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("%v", err)
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
	if err := s.AddSpare(retired, noMax); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(retired); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.Seal(retired, true); err != nil {
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
	if err := s.AddSpare(stillActive, noMax); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(stillActive); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if s.Claim(stillActive) {
		t.Fatal("Claim on an Active segment was held, want it refused")
	}
	if err := s.Seal(stillActive, true); err != nil {
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
	if err := s.AddSpare(sealedEarly, noMax); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(sealedEarly); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.Seal(sealedEarly, false); err != nil {
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
	if err := s.AddSpare(ended, noMax); err != nil {
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
	if err := s.AddSpare(torn, noMax); err != nil {
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
	if err := s.AddSpare(growing, noMax); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(growing); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.Seal(growing, false); err != nil {
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

// TestRetireKeepsADroppedShareTotalForTheCheck: nothing in the ledger's own
// operation can take a share's total below what its entries hold, so a
// negative total, reachable only from an accounting bug, is kept rather than
// deleted — deleting it as if it were the zero a settled share reaches would
// hide the drop from the check. The retire that found it leaves the key in
// the map, and the check reports the total the entries cannot make up.
func TestRetireKeepsADroppedShareTotalForTheCheck(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	one, two := core.SegmentID(1), core.SegmentID(2)
	for _, id := range []core.SegmentID{one, two} {
		if err := s.AddSpare(id, noMax); err != nil {
			t.Fatalf("AddSpare %d: %v", id, err)
		}
		if err := s.Take(id); err != nil {
			t.Fatalf("Take %d: %v", id, err)
		}
	}
	tag := core.ShareTag(4)
	if err := s.AddRecord(one, tag, 100); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if err := s.AddRecord(two, tag, 60); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if err := s.Seal(one, true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := s.Seal(two, true); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := s.Check(); err != nil {
		t.Fatalf("the consistent set: %v", err)
	}

	// A share total dropped below what its entries hold: the drop is an
	// accounting bug the retire's release is about to take negative. Only a
	// total that ends a release below zero exercises the branch that keeps
	// it; a positive leftover is kept by any implementation of the rule.
	s.mu.Lock()
	s.tags[tag] -= segCap // far below both segments' bytes together
	s.mu.Unlock()

	// The retire of the smaller holder takes its share of the tag negative,
	// so the key stays — deleting the leftover as if it were the zero a
	// settled share reaches would hide the drop from the check.
	if _, err := s.Retire(two); err != nil {
		t.Fatalf("the retire of the smaller holder: %v", err)
	}
	_, kept := s.tags[tag]
	if !kept {
		t.Fatal("the negative share total was deleted by the retire's release, want it kept for the check")
	}
	if err := s.Check(); !errors.Is(err, ErrMismatch) {
		t.Fatalf("the kept negative total: %v, want it to wrap %v", err, ErrMismatch)
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
	if err := s.AddSpare(id, noMax); err != nil {
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
	if err := s.Seal(id, true); err != nil {
		t.Fatalf("Seal after the refused retire: %v", err)
	}
	if _, err := s.Retire(id); err != nil {
		t.Fatalf("the retire after sealing: %v", err)
	}
}

// TestCheckComparesTheEntriesHeldWithTheirTags: the running total of a
// segment's record bytes is bound to the bytes its own per-tag map holds and
// to its accounted size. A drifted total admits a record that does not fit —
// AddRecord trusts it as the bound — so the check sums the entries' tags and
// reports the drift rather than taking the total's word.
func TestCheckComparesTheEntriesHeldWithTheirTags(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	s := NewSet(segCap, nil)
	id := core.SegmentID(1)
	tag := core.ShareTag(2)
	if err := s.AddSpare(id, noMax); err != nil {
		t.Fatalf("AddSpare: %v", err)
	}
	if err := s.Take(id); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := s.AddRecord(id, tag, 4000); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}

	// The total drifts to nothing while the entry still holds its 4000: the
	// next record overfills the segment past its size unnoticed.
	s.mu.Lock()
	s.segs[id].held = 0
	s.mu.Unlock()
	if err := s.AddRecord(id, tag, 4000); err != nil {
		t.Fatalf("AddRecord after the drift: %v, want it admitted: the drift is the bug, not the record", err)
	}
	err := s.Check()
	if err == nil {
		t.Fatal("a drifted held total was not reported")
	}
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("the drifted total was reported as %v, want it to wrap %v", err, ErrMismatch)
	}

	// Over its size is the other half of the same clause: the total may not
	// stand above the storage the segment is accounted at. The entry's own
	// tags make exactly the same figure, and the share's total follows, so the
	// tags half of the clause — and the global share comparison — have
	// nothing to disagree with and only the size half can report.
	s.mu.Lock()
	e := s.segs[id]
	e.held = e.size + 1
	e.tags[tag] = e.size + 1
	s.segs[id] = e
	s.tags[tag] = e.size + 1
	s.mu.Unlock()
	if err := s.Check(); err == nil {
		t.Fatal("a held total above the size was not reported")
	} else if !errors.Is(err, ErrMismatch) {
		t.Fatalf("a held total above the size: %v, want it to wrap %v", err, ErrMismatch)
	}
}

// TestCheckComparesEveryMaintainedCountWithTheSet: each counter the ledger
// maintains — the footprint, the segments, the sealed, the spares — is one the
// check re-derives by walking the set, and the shares' totals are re-derived
// from the entries' own tags. A counter that drifted from what the set's own
// state makes of it is a broken ledger: the check names it, because the
// maintained figures are precisely the ones nothing recomputes at read time.
func TestCheckComparesEveryMaintainedCountWithTheSet(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const segCap = 4096
	// A ledger holding one spare, one sealed and one active segment with
	// records of one share: every count the check re-derives has something to
	// disagree with once its counter drifts.
	drift := func(name, says string, corrupt func(*Set)) {
		s := NewSet(segCap, nil)
		spares := []core.SegmentID{1, 2}
		for _, id := range spares {
			if err := s.AddSpare(id, noMax); err != nil {
				t.Fatalf("AddSpare %d: %v", id, err)
			}
		}
		if err := s.Take(1); err != nil {
			t.Fatalf("Take: %v", err)
		}
		if err := s.Take(2); err != nil {
			t.Fatalf("Take: %v", err)
		}
		if err := s.AddRecord(2, core.ShareTag(3), 400); err != nil {
			t.Fatalf("AddRecord: %v", err)
		}
		if err := s.Seal(1, true); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if err := s.Check(); err != nil {
			t.Fatalf("the consistent set: %v", err)
		}

		s.mu.Lock()
		corrupt(s)
		s.mu.Unlock()
		if err := s.Check(); err == nil {
			t.Fatalf("a drifted %s was not reported", name)
		} else if !errors.Is(err, ErrMismatch) {
			t.Fatalf("a drifted %s was reported as %v, want it to wrap %v", name, err, ErrMismatch)
		} else if !strings.Contains(err.Error(), says) {
			t.Fatalf("a drifted %s was reported as %q, want it to come from the branch that names it (containing %q)", name, err, says)
		}
	}
	for _, c := range []struct {
		name    string
		corrupt func(*Set)
		// says is a fragment the drift's own error branch carries: the check
		// names the drifted figure, not just any mismatch it can find.
		says string
	}{
		{"footprint", func(s *Set) { s.c.UsedBytes -= segCap }, "the set's sizes make"},
		{"segment count", func(s *Set) { s.c.Segments-- }, "counts do not match"},
		{"sealed count", func(s *Set) { s.c.Sealed++ }, "counts do not match"},
		{"spare count", func(s *Set) { s.c.Spares++ }, "counts do not match"},
		// no entry charges it, so only the shares' totals comparison can
		// report it
		{"share total", func(s *Set) { s.tags[core.ShareTag(9)] = 1 }, "shares' record bytes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			drift(c.name, c.says, c.corrupt)
		})
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
