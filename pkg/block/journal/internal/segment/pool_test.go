package segment_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journal/internal/segment"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest/prof"
)

// poolAttempts bounds the polls a test makes on the pool before it gives up:
// a count of yields, never a sleep — the refill worker is a goroutine the test
// cannot hold, so a test watches the counters move instead of sleeping.
const poolAttempts = 1 << 22

// poolOf is a pool over a fresh seam and ledger: the Maker seeded with last,
// reserving against max, the pool keeping spares of t ready. The set the pool
// reaches through the Maker is the set returned beside it, so a test reads
// the ledger's counters directly.
func poolOf(t *testing.T, t2 segment.Targets, max int64, last core.SegmentID, log *slog.Logger) (
	*journaltest.Seam, *segment.Set, *segment.Maker, *segment.Pool,
) {
	t.Helper()

	seam := newSeam(t)
	set := segment.NewSet(segCap, nil)
	m := segment.NewMaker(seam.FS(), set, max, last, log)
	return seam, set, m, segment.NewPool(m, t2, log)
}

// awaitCounters fails t unless what the pool reports satisfies want within a
// bounded count of yields.
func awaitCounters(t *testing.T, what string, p *segment.Pool, want func(segment.PoolCounters) bool) {
	t.Helper()

	for range poolAttempts {
		if c := p.Counters(); want(c) {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("the pool never reached %s: it reports %+v", what, p.Counters())
}

// awaitDeviceFull fails t unless DeviceFull reports want within a bounded
// count of yields: one make's outcome lands in the state only once the
// worker returns.
func awaitDeviceFull(t *testing.T, want bool, p *segment.Pool) {
	t.Helper()

	for range poolAttempts {
		if p.DeviceFull() == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("the pool's device-full state never became %v", want)
}

// assertSpareRefusal fails t unless err is the NoSpaceError the spare pool
// owes: the spare limit, missing segments, the segments unit. Every refused
// take a test checks also checks the two spellings of the limit's own.
func assertSpareRefusal(t *testing.T, where string, err error, missing int64) {
	t.Helper()

	var ns *core.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("%s: %v, want a *core.NoSpaceError", where, err)
	}
	if ns.Limit != core.LimitSpare {
		t.Fatalf("%s named the %s limit, want %s", where, ns.Limit, core.LimitSpare)
	}
	if ns.Missing != missing {
		t.Fatalf("%s said %d missing, want %d", where, ns.Missing, missing)
	}
	if ns.Unit != core.UnitSegments {
		t.Fatalf("%s counted %s, want %s", where, ns.Unit, core.UnitSegments)
	}
}

// recording is a slog Handler keeping every record handed to it, so a test
// can assert the pool logged a refusal and at which level. The refill worker
// logs from its own goroutine, so the records are kept under a mutex.
type logRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, msg slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, msg.Clone())
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

// countAt returns how many records the handler holds at level whose message
// contains sub.
func (r *logRecorder) countAt(level slog.Level, sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	for _, rec := range r.recs {
		if rec.Level == level && strings.Contains(rec.Message, sub) {
			n++
		}
	}
	return n
}

// awaitLogged fails t unless the handler holds n records at level whose
// message contains sub, within a bounded count of yields: the worker logs
// from its own goroutine, the test cannot hold.
func awaitLogged(t *testing.T, h slog.Handler, level slog.Level, sub string, n int) {
	t.Helper()

	recs, ok := h.(*logRecorder)
	if !ok {
		t.Fatalf("awaitLogged: the handler is not the test's recorder")
	}
	for range poolAttempts {
		if got := recs.countAt(level, sub); got >= n {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("the pool never logged %d records at %s containing %q; the handler holds %d",
		n, level, sub, recs.countAt(level, sub))
}

// TestPoolContentTakeNeverTakesHeadroomOrReserve: a content take is refused
// `spare` when only the headroom's and the reserve's spares are ready, while
// those two classes' takes are served — the classes never lend to one
// another, because the headroom and the reserve are what keep a full journal
// able to free space. It catches a class fallback that trades a permanent
// wedge for one served roll.
func TestPoolContentTakeNeverTakesHeadroomOrReserve(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, set, _, p := poolOf(t, segment.Targets{Content: 0, Headroom: 2, Reserve: 1}, noMax, 0, nil)
	p.Start()

	// The fill makes the headroom's two and the reserve's one; content keeps
	// none, so the fill stops there.
	awaitCounters(t, "headroom 2 and reserve 1 ready", p,
		func(c segment.PoolCounters) bool {
			return c.HeadroomBytes == 2*segCap && c.RepackReserveBytes == segCap && c.Spares == 0
		})
	before := set.Counters()

	_, err := p.Take(segment.Content)
	assertSpareRefusal(t, "the content take over the other classes", err, 1)

	if id, err := p.Take(segment.Headroom); err != nil || id == 0 {
		t.Fatalf("the headroom take over a ready headroom spare: (%d, %v)", id, err)
	}
	if id, err := p.Take(segment.Reserve); err != nil || id == 0 {
		t.Fatalf("the reserve take over a ready reserve spare: (%d, %v)", id, err)
	}

	c := set.Counters()
	if c.Segments != 2 {
		t.Fatalf("after the two takes the ledger holds %d segments, want 2", c.Segments)
	}
	if c.UsedBytes != before.UsedBytes {
		t.Fatalf("the takes moved the footprint to %d, want the %d of the fill alone: a take allocates nothing",
			c.UsedBytes, before.UsedBytes)
	}
	if err := set.Check(); err != nil {
		t.Fatalf("after the takes: %v", err)
	}

	p.Stop()
	if got := seam.Calls(journaltest.OpCreate); got != 3 {
		t.Fatalf("the pool made %d files for targets of 3, want 3", got)
	}
}

// TestPoolTakeMakesNoSeamCall: a take costs only the take. It catches a take
// that makes a segment on the write path, whose call counts would move while
// the write it serves is in flight. The pool sits at the journal's maximum,
// so the refill the take's kick wakes cannot make a new spare — its make is
// refused by the ledger before any storage is touched — while the take itself
// succeeds over the one ready spare.
func TestPoolTakeMakesNoSeamCall(t *testing.T) {
	prof.Watch(t, prof.Options{})

	h := &logRecorder{}
	log := slog.New(h)
	seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, segCap, 0, log)
	p.Start()
	awaitCounters(t, "the content spare ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })

	ops := []string{
		journaltest.OpCreate,
		journaltest.OpAllocate,
		journaltest.OpSync,
		journaltest.OpRename,
		journaltest.OpSyncDir,
	}
	before := make(map[string]int, len(ops))
	for _, op := range ops {
		before[op] = seam.Calls(op)
	}

	id, err := p.Take(segment.Content)
	if err != nil || id == 0 {
		t.Fatalf("the take over the ready spare: (%d, %v)", id, err)
	}

	// The take kicked the worker, whose make the maximum refuses before any
	// storage is touched: the Debug record is what proves the pass ran and
	// still no call count moved.
	awaitLogged(t, h, slog.LevelDebug, "did not make a spare", 1)

	for _, op := range ops {
		if got := seam.Calls(op); got != before[op] {
			t.Fatalf("after the take of spare %d, %s was called %d times, want still %d: a take makes no storage call",
				id, op, got, before[op])
		}
	}
	p.Stop()
}

// TestPoolTakeFindsNoSpareRefusedAtOnce: a take that finds no spare is
// refused at once, never waiting for the refill's make in flight — space is
// the caller's to free, so a take that waited would wait on the make it
// cannot hold. The worker's create is held at the seam when the take runs;
// the take must return the spare refusal before the hold ever opens. It
// catches a take that blocks on the pool's filling.
func TestPoolTakeFindsNoSpareRefusedAtOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)
	// "*" holds the create whichever name the worker drew, which the test
	// cannot know: the id is drawn inside the worker's own make.
	h := seam.HoldFirst(t, journaltest.OpCreate, "*")
	p.Start()

	// The worker is parked at its first create; the pool holds no content
	// spare. The take must answer at once: the hold proves the make has begun,
	// so a take that waited on it would still be blocked here when the
	// refusal is checked.
	done := make(chan error, 1)
	go func() { _, err := p.Take(segment.Content); done <- err }()

	select {
	case err := <-done:
		assertSpareRefusal(t, "the take with the make held", err, 1)
	case <-h.Reached():
		select {
		case err := <-done:
			assertSpareRefusal(t, "the take with the make held at the seam", err, 1)
		case <-time.After(10 * time.Second):
			t.Fatal("the take is blocked on the held make, want the refusal at once")
		}
	}

	h.Release()
	p.Stop()
}

// TestPoolHoldKeepsSparesFromUnreservedTake: a hold's spares are not
// available to unreserved takes. The journal's maximum holds exactly the
// content target, so no refill can ever raise the pool above it and the
// test's counts stand on the takes alone, not on a race with the worker.
// A Hold(2) over the pool's two spares owns both: the unreserved take is
// refused `spare` while the hold's own two takes succeed, and a hold asking
// beyond what is ready is refused with the count it fell short by. It catches
// a hold that accounts its takes but not the spares it owes, which would
// serve a reserved write its own spare's refusal.
func TestPoolHoldKeepsSparesFromUnreservedTake(t *testing.T) {
	prof.Watch(t, prof.Options{})

	_, set, _, p := poolOf(t, segment.Targets{Content: 2}, 2*segCap, 0, nil)
	p.Start()
	awaitCounters(t, "the two content spares ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	h, err := p.Hold(2)
	if err != nil {
		t.Fatalf("Hold(2) over two ready spares: %v", err)
	}
	defer h.Close()
	if c := p.Counters(); c.Held != 2 {
		t.Fatalf("after the hold the pool owes %d spares, want 2", c.Held)
	}

	// The two spares are the hold's: an unreserved take is refused.
	_, err = p.Take(segment.Content)
	assertSpareRefusal(t, "the unreserved take over a two-spare hold", err, 1)

	// The hold takes both, ahead of every unreserved write.
	for i := 1; i <= 2; i++ {
		id, err := h.Take()
		if err != nil || id == 0 {
			t.Fatalf("the hold's take %d: (%d, %v)", i, id, err)
		}
	}
	if c := p.Counters(); c.Held != 0 || c.Spares != 0 {
		t.Fatalf("after the hold's takes the counters are %+v, want no spare ready and none owed", c)
	}
	if c := set.Counters(); c.Segments != 2 || c.Spares != 0 {
		t.Fatalf("after the hold's takes the ledger holds %d segments and %d spares, want 2 and 0", c.Segments, c.Spares)
	}

	// A hold beyond what is left is refused with what it fell short by. The
	// maximum holds nothing more, so the pool stands at 0 ready and a
	// Hold(1) is a whole hold short: 1 missing. A Hold(3) over nothing is
	// short 3.
	_, err = p.Hold(1)
	assertSpareRefusal(t, "the hold of 1 over none ready", err, 1)
	_, err = p.Hold(3)
	assertSpareRefusal(t, "the hold of 3 over none ready", err, 3)
	if c := p.Counters(); c.Held != 0 {
		t.Fatalf("after the refused holds the pool owes %d spares, want none: a refusal changes nothing", c.Held)
	}
	p.Stop()
}

// TestPoolHoldReplaceGoesAheadOfUnreserved: a replacement goes to the
// reservation ahead of every unreserved write. The worker's makes are parked
// at the seam — no spare can be made, but the pool itself stays open — and
// the hold of one takes its spare and replaces it. The test allows one spare
// to be made, so the replace leaves the pool holding one ready spare that
// the unreserved take is still refused, because the pool owes that spare to
// the reservation, while the hold's own take succeeds. It catches a
// replacement that cannot outrank an unreserved take, and a hold whose Close
// leaves its spares owed.
func TestPoolHoldReplaceGoesAheadOfUnreserved(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, set, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)
	p.Start()
	awaitCounters(t, "the one content spare ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })

	// The pool's only made spare is the one the target holds, so the worker
	// parks at its next create: the takes' kicks hold it there, and nothing
	// refills until the last one is freed.
	h := seam.HoldFirst(t, journaltest.OpCreate, "*")

	hold, err := p.Hold(1)
	if err != nil {
		t.Fatalf("Hold(1) over the ready spare: %v", err)
	}
	if _, err := hold.Take(); err != nil {
		t.Fatalf("the hold's take: %v", err)
	}
	hold.Replace()
	if c := p.Counters(); c.Spares != 0 || c.Held != 1 {
		t.Fatalf("after the replace the counters are %+v, want none ready and 1 owed", c)
	}

	// The take's kick has the worker parked at its create — no spare is
	// ready and none can be made while it is held — and the unreserved take
	// is refused: the pool owes its next spare to the reservation.
	_, err = p.Take(segment.Content)
	assertSpareRefusal(t, "the unreserved take with the pool short", err, 1)

	// The hold's take is refused too, while no spare is ready: a reserved
	// write never meets the no-spare refusal while the pool can be refilled,
	// and while it cannot, neither does it go ahead of one.
	_, err = hold.Take()
	assertSpareRefusal(t, "the hold's take with no spare ready", err, 1)

	// Freed, the make runs, and the hold's take goes through — the
	// replacement is its spare. The unreserved take, having been refused
	// while this spare was owed to the hold, never saw it.
	h.Release()
	awaitCounters(t, "the replaced spare made", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })
	if id, err := hold.Take(); err != nil || id == 0 {
		t.Fatalf("the hold's take over the replaced spare: (%d, %v)", id, err)
	}
	hold.Close()
	if c := p.Counters(); c.Held != 0 {
		t.Fatalf("after the close the pool owes %d spares, want none: the close returns them", c.Held)
	}
	if _, err := hold.Take(); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the closed hold's take: %v, want ErrClosed", err)
	}
	if err := set.Check(); err != nil {
		t.Fatalf("after the holds: %v", err)
	}
	p.Stop()
}

// TestPoolAdoptCountsTowardClassAtCap: a spare found at open is adopted into
// its class at the cap. Three spare files are made over a second pair of
// seam and ledger — the open the test stands in for — and adopted one per
// class into a fresh set and pool: the counters report {content 1, cap
// bytes, cap bytes}, the ledger's footprint holds all three at the cap, and
// the check passes. Adopt also works over a maximum below the footprint —
// the file already exists, so a lowered maximum refuses only new spares —
// and is refused once the pool has started, because open syncs the
// directory before any re-adopted spare is taken. A Take before Start is
// refused as well: the pool hands nothing out before that sync returned. It
// catches an adoption that accounts no cap, and one a take could reach before
// the directory sync.
func TestPoolAdoptCountsTowardClassAtCap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The "directory at open": three whole spare files made over a ledger
	// the pool under test never reaches.
	seam, set2, m2 := maker(t, 0, noMax)
	ids := make([]core.SegmentID, 3)
	for i := range ids {
		id, err := m2.Make()
		if err != nil {
			t.Fatalf("the spare's making %d: %v", i, err)
		}
		ids[i] = id
	}
	// What open does with the maker's ledger: its spares leave it, for open
	// adopts the files into the journal's own.
	for _, id := range ids {
		if err := set2.Take(id); err != nil {
			t.Fatalf("the directory's ledger: Take %d: %v", id, err)
		}
	}

	// The journal's own ledger and pool, at a maximum one cap below the
	// footprint the adoptions make: adopting is still served.
	_, set, m := maker(t, 4, 2*segCap)
	p := segment.NewPool(m, segment.Targets{Content: 1, Headroom: 1, Reserve: 1}, nil)

	classes := []struct {
		id  core.SegmentID
		cls segment.Class
	}{
		{ids[0], segment.Content},
		{ids[1], segment.Headroom},
		{ids[2], segment.Reserve},
	}
	for _, tc := range classes {
		if err := p.Adopt(tc.id, tc.cls); err != nil {
			t.Fatalf("the adoption of %d into %s over the lowered maximum: %v", tc.id, tc.cls, err)
		}
	}

	c := p.Counters()
	if c.Spares != 1 || c.HeadroomBytes != segCap || c.RepackReserveBytes != segCap {
		t.Fatalf("after the adoptions the pool reports %+v, want one content spare, cap bytes of headroom and cap bytes of reserve", c)
	}
	l := set.Counters()
	if l.UsedBytes != 3*segCap || l.Spares != 3 {
		t.Fatalf("after the adoptions the ledger holds %d bytes in %d spares, want three caps in 3", l.UsedBytes, l.Spares)
	}
	if err := set.Check(); err != nil {
		t.Fatalf("after the adoptions: %v", err)
	}

	// The ids the file already holds are refused by the ledger, not the
	// pool's classes: a spare is never adopted twice.
	if err := p.Adopt(ids[0], segment.Content); !errors.Is(err, segment.ErrDuplicateID) {
		t.Fatalf("the second adoption of %d: %v, want ErrDuplicateID", ids[0], err)
	}
	if err := p.Adopt(0, segment.Content); !errors.Is(err, segment.ErrBadID) {
		t.Fatalf("the adoption of the null id: %v, want ErrBadID", err)
	}
	var bad segment.Class
	if err := p.Adopt(core.SegmentID(7), bad); !errors.Is(err, segment.ErrBadClass) {
		t.Fatalf("the adoption of a bad class: %v, want ErrBadClass", err)
	}

	// Before Start, nothing is taken: the directory sync has not returned.
	_, err := p.Take(segment.Content)
	assertSpareRefusal(t, "the take before Start", err, 1)
	if _, err := p.Hold(1); err == nil {
		t.Fatal("a hold before Start succeeded, want the spare refusal")
	} else {
		assertSpareRefusal(t, "the hold before Start", err, 1)
	}

	p.Start()
	if err := p.Adopt(core.SegmentID(9), segment.Content); !errors.Is(err, segment.ErrStarted) {
		t.Fatalf("the adoption after Start: %v, want ErrStarted", err)
	}

	// What was adopted is taken: the content spare the journal found at open
	// is its next segment.
	id, err := p.Take(segment.Content)
	if err != nil || id != ids[0] {
		t.Fatalf("the take of the adopted content spare: (%d, %v), want %d", id, err, ids[0])
	}
	if l := set.Counters(); l.Segments != 1 || l.Spares != 2 {
		t.Fatalf("after the take the ledger holds %d segments and %d spares, want 1 and 2", l.Segments, l.Spares)
	}
	if err := set.Check(); err != nil {
		t.Fatalf("after the take: %v", err)
	}
	p.Stop()
	_ = seam
}

// TestPoolBytesFiguresFollowTheClasses: the headroom and reserve figures
// follow the classes' spares at the cap, as allocated now. After the fill at
// targets {1, 2, 3}, the headroom holds two spares' worth and the reserve
// three; a headroom take takes the figure with the spare until the refill
// replaces it. The two classes' spares are journal-accounted: charged to no
// share's tag, sitting in the ledger's own spare count, which holds all six.
// It catches a figure that counts a class as bytes it no longer holds, and a
// class spare charged to a share.
func TestPoolBytesFiguresFollowTheClasses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	_, set, _, p := poolOf(t, segment.Targets{Content: 1, Headroom: 2, Reserve: 3}, noMax, 0, nil)
	p.Start()
	awaitCounters(t, "targets {1,2,3} filled", p,
		func(c segment.PoolCounters) bool {
			return c.Spares == 1 && c.HeadroomBytes == 2*segCap && c.RepackReserveBytes == 3*segCap
		})

	// The classes' spares are journal-accounted: no share's tag holds a
	// record byte of them, and the ledger's own spare count holds all six.
	if c := set.Counters(); c.Spares != 6 {
		t.Fatalf("after the fill the ledger holds %d spares, want 6", c.Spares)
	}
	for _, tag := range []core.ShareTag{1, 2, 42} {
		if got := set.ShareBytes(tag); got != 0 {
			t.Fatalf("share tag %d holds %d record bytes, want none: a spare is charged to no share", tag, got)
		}
	}

	if id, err := p.Take(segment.Headroom); err != nil || id == 0 {
		t.Fatalf("the headroom take: (%d, %v)", id, err)
	}
	awaitCounters(t, "the headroom taken down to cap", p,
		func(c segment.PoolCounters) bool { return c.HeadroomBytes == segCap })
	// The take is still in flight with the fill: the figure rides the class
	// back to its target once the worker has made the spare.
	awaitCounters(t, "the headroom refilled to two", p,
		func(c segment.PoolCounters) bool { return c.HeadroomBytes == 2*segCap })
	if err := set.Check(); err != nil {
		t.Fatalf("after the take: %v", err)
	}
	p.Stop()
}

// TestPoolDeviceFullOnENOSPC: the state a roll reads is set by the ENOSPC of
// a failed make and cleared by the first make that succeeds. With every
// allocation failing ENOSPC, DeviceFull turns true; the fault cleared and the
// worker kicked, one make succeeds and the state is false again. With an EIO
// instead, the state stays false: a wrong device is a fault to name, not a
// full one. It catches a state that never clears, and one set by any error
// that is not ENOSPC.
func TestPoolDeviceFullOnENOSPC(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("ENOSPC sets it and a made spare clears it", func(t *testing.T) {
		seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)
		if err := seam.Schedule(journaltest.Failing(journaltest.OpAllocate, "*", 0, syscall.ENOSPC)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		p.Start()
		awaitDeviceFull(t, true, p)

		if err := seam.Schedule(); err != nil {
			t.Fatalf("the cleared Schedule: %v", err)
		}
		p.Kick()
		awaitCounters(t, "the spare made over the cleared fault", p,
			func(c segment.PoolCounters) bool { return c.Spares == 1 })
		awaitDeviceFull(t, false, p)
		p.Stop()
	})

	t.Run("EIO sets nothing", func(t *testing.T) {
		seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)
		if err := seam.Schedule(journaltest.Failing(journaltest.OpAllocate, "*", 0, syscall.EIO)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		p.Start()

		// The worker has run its pass and failed it: bounded, because the
		// failure itself is what must land, and the take's kick is what
		// proves the pass ran again.
		if _, err := p.Take(segment.Content); err == nil {
			t.Fatal("a take over the faulted pool succeeded, want the spare refusal")
		}
		if p.DeviceFull() {
			t.Fatal("the EIO set the device-full state, want it left false")
		}
		p.Stop()
	})
}

// TestPoolRefillRefusedByMaximumStopsThePass: a pass the journal maximum
// refuses stops, one make per kick, and does not turn the refusal into a
// full device: with a maximum of two caps and a content target of 3, exactly
// two spares are made and no third create is attempted, while DeviceFull
// stays false — the maximum is the pool working as configured, not a device
// that has run out. It catches a busy loop hammering the reservation, and a
// maximum mistaken for ENOSPC.
func TestPoolRefillRefusedByMaximumStopsThePass(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, _, _, p := poolOf(t, segment.Targets{Content: 3}, 2*segCap, 0, nil)
	p.Start()
	awaitCounters(t, "the two spares the maximum holds", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	// The kick of a take (or the worker's own end of pass — there is none
	// here, the pass stopped on the refusal) must not make a third create.
	for range 3 {
		if _, err := p.Take(segment.Content); err != nil && !errors.Is(err, core.ErrClosed) {
			// Takes succeed here; the kicks are what matters.
			_ = err
		}
		p.Kick()
	}
	p.Stop()

	if got := seam.Calls(journaltest.OpCreate); got != 2 {
		t.Fatalf("after the kicks the pool made %d files, want 2: a refused reservation makes no storage call",
			got)
	}
	if p.DeviceFull() {
		t.Fatal("a maximum's refusal set the device-full state, want it false")
	}
}

// TestPoolFillOrderHeadroomReserveContent: a pass fills the headroom first,
// then the reserve, then the content class — the headroom and the reserve
// are what keep a full journal able to free space, so a pass that made a
// content spare while a reserve sat short would trade a permanent wedge for
// one more served roll. The ids the Maker draws ascend as it makes, so the
// headroom's spare is the lowest id, the reserve's the next, the content's
// the highest. It catches a fill order that leaves the reserves for last.
func TestPoolFillOrderHeadroomReserveContent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	_, set, _, p := poolOf(t, segment.Targets{Content: 1, Headroom: 1, Reserve: 1}, noMax, 0, nil)
	p.Start()
	awaitCounters(t, "targets {1,1,1} filled", p,
		func(c segment.PoolCounters) bool {
			return c.Spares == 1 && c.HeadroomBytes == segCap && c.RepackReserveBytes == segCap
		})

	// The last pool-reported takeable moment: hold the worker's next create
	// so no refill disturbs the ids, and read them by taking the classes.
	// The content spare is the pool's only one; the headroom's and the
	// reserve's are taken from their classes. A take moves the spare into
	// the set as a segment, whose id is what the test reads.
	head := takeClass(t, p, segment.Headroom)
	res := takeClass(t, p, segment.Reserve)
	con := takeClass(t, p, segment.Content)

	if head >= res || res >= con {
		t.Fatalf("the fill drew the headroom %d, the reserve %d and the content %d, want them ascending in that order",
			head, res, con)
	}
	if c := set.Counters(); c.Segments != 3 {
		t.Fatalf("after the three takes the ledger holds %d segments, want 3", c.Segments)
	}
	p.Stop()
}

// takeClass takes the one spare the class holds, refusing the test on any
// error: the helper of the fill-order test alone, where every class holds
// exactly one spare.
func takeClass(t *testing.T, p *segment.Pool, c segment.Class) core.SegmentID {
	t.Helper()

	id, err := p.Take(c)
	if err != nil || id == 0 {
		t.Fatalf("the take of the %s class: (%d, %v)", c, id, err)
	}
	return id
}

// TestPoolStopJoinsWorkerAndKeepsSpares: Stop joins the worker — a make in
// flight when Stop is called completes first, and Stop returns only after
// the worker has exited — and removes nothing: the untaken spares stay on
// disk and in the ledger for the next open to re-adopt, the take after Stop
// answers ErrClosed, and no remove call was ever made. It catches a Stop
// that leaves the worker's make running behind it and one that unlinks the
// spares it holds.
func TestPoolStopJoinsWorkerAndKeepsSpares(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, set, _, p := poolOf(t, segment.Targets{Content: 2}, noMax, 0, nil)

	// Both holds are registered before the worker starts, so the sequence is
	// the scheduler's to keep, not a race: h1 parks the first create — its
	// release also lets every later call pass — and h2, registered before
	// that release, parks the second create, the make Stop must join.
	h1 := seam.Hold(t, journaltest.OpCreate, "*")
	p.Start()
	<-h1.Reached()
	h2 := seam.HoldFirst(t, journaltest.OpCreate, "*")
	h1.Release()
	awaitCounters(t, "the first spare ready while the second is parked", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })
	<-h2.Reached()

	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()

	select {
	case <-stopped:
		t.Fatal("Stop returned while its worker's make was still held at the seam, want the join")
	case <-time.After(100 * time.Millisecond):
	}
	h2.Release()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop never returned once the held make finished, want the join right after")
	}

	if got := seam.Calls(journaltest.OpRemove); got != 0 {
		t.Fatalf("Stop removed %d files, want none: the untaken spares are the next open's to re-adopt",
			got)
	}
	if _, err := p.Take(segment.Content); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the take after Stop: %v, want ErrClosed", err)
	}
	if _, err := p.Hold(1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the hold after Stop: %v, want ErrClosed", err)
	}
	p.Stop()
	if err := set.Check(); err != nil {
		t.Fatalf("after Stop: %v", err)
	}

	// The spares stay in the ledger: the one taken by nothing and one the
	// worker may have finished while Stop joined — whichever way the held
	// create ended, the check above holds and no spare left the ledger
	// but through a take, of which there was none after the first.
	c := set.Counters()
	if c.Spares < 1 {
		t.Fatalf("after Stop the ledger holds %d spares, want at least the one the pool had ready", c.Spares)
	}
	if c.Segments != 0 {
		t.Fatalf("after Stop the ledger holds %d segments, want none: nothing was taken", c.Segments)
	}
}

// TestPoolRaceTakesHoldsAndRefill: takes, holds and the refill over one pool
// under the race detector. Eight goroutines loop a bounded count of
// unreserved content takes — tolerating the spare refusal, which is what the
// pool owes — holds that take and close, and kicks. At the end the ledger
// checks, no id was handed out twice, and the pool holds no spares it owes.
func TestPoolRaceTakesHoldsAndRefill(t *testing.T) {
	prof.Watch(t, prof.Options{})

	_, set, _, p := poolOf(t, segment.Targets{Content: 4, Headroom: 1, Reserve: 1}, noMax, 0, nil)
	p.Start()

	const (
		goroutines = 8
		rounds     = 64
	)
	var (
		mu   sync.Mutex
		seen = make(map[core.SegmentID]bool)
		wg   sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				switch g % 3 {
				case 0:
					id, err := p.Take(segment.Content)
					if err == nil {
						mu.Lock()
						if seen[id] {
							mu.Unlock()
							panic("the pool handed one spare to two takes")
						}
						seen[id] = true
						mu.Unlock()
					}
				case 1:
					h, err := p.Hold(1)
					if err == nil {
						id, terr := h.Take()
						var ns *core.NoSpaceError
						if terr != nil && !errors.As(terr, &ns) && !errors.Is(terr, core.ErrClosed) {
							panic(fmt.Sprintf("the hold's take: %v", terr))
						}
						if terr == nil {
							mu.Lock()
							if seen[id] {
								mu.Unlock()
								panic("the pool handed one spare to two takes")
							}
							seen[id] = true
							mu.Unlock()
						}
						h.Close()
					}
				case 2:
					p.Kick()
				}
			}
		}(g)
	}
	wg.Wait()
	p.Stop()

	if err := set.Check(); err != nil {
		t.Fatalf("the raced pool left the ledger: %v", err)
	}
	if c := p.Counters(); c.Held != 0 {
		t.Fatalf("after the race the pool owes %d spares, want none: every hold closed", c.Held)
	}
	l := set.Counters()
	if l.Segments != int64(len(seen)) {
		t.Fatalf("the pool handed %d ids out while the ledger holds %d segments, want them equal",
			len(seen), l.Segments)
	}
}
