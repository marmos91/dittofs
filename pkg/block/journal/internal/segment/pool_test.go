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
// able to free space. The journal's maximum is pinned at the fill, so every
// refill the takes kick is refused by the ledger before any storage is
// touched and the counts below stand on the takes alone, never on a race
// with the worker. It catches a class fallback that trades a permanent
// wedge for one served roll.
func TestPoolContentTakeNeverTakesHeadroomOrReserve(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, set, _, p := poolOf(t, segment.Targets{Content: 0, Headroom: 2, Reserve: 1}, 3*segCap, 0, nil)
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

// TestPoolTakeMakesNoSeamCall: a take costs only the take — it performs no
// storage call itself, however short the pool sits behind it. The take's
// return happens while the worker's next create is held at the seam, so a
// take that made its own spare would block on the hold it can never pass; the
// worker's own create is then the only storage call that moved, and it moved
// once. It catches a make on the write path that a pinned maximum would have
// hidden, because a take-side make would be refused by the ledger before the
// seam and leave every count untouched either way.
func TestPoolTakeMakesNoSeamCall(t *testing.T) {
	prof.Watch(t, prof.Options{})

	h := &logRecorder{}
	log := slog.New(h)
	// No maximum: the pool can always make another spare, so the counts
	// below prove the take made none rather than the ledger refusing one.
	seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, log)
	p.Start()
	awaitCounters(t, "the content spare ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })

	// The worker's next create is parked at the seam, so whatever runs after
	// this point and makes anything blocks where the test can see it.
	hold := seam.HoldFirst(t, journaltest.OpCreate, "*")

	ops := []string{
		journaltest.OpAllocate,
		journaltest.OpSync,
		journaltest.OpRename,
		journaltest.OpSyncDir,
	}
	before := make(map[string]int, len(ops))
	for _, op := range ops {
		before[op] = seam.Calls(op)
	}
	creates := seam.Calls(journaltest.OpCreate)

	// The take must return while the worker's create is held: a take that
	// made its own spare would wait on this hold and never answer.
	id, err := p.Take(segment.Content)
	if err != nil || id == 0 {
		t.Fatalf("the take over the ready spare while the worker is parked: (%d, %v)", id, err)
	}

	// The take's kick woke the worker, whose own create is the held one.
	<-hold.Reached()

	// The worker's create is the only call that moved — the take itself made
	// no seam call — and it moved exactly once.
	if got := seam.Calls(journaltest.OpCreate); got != creates+1 {
		t.Fatalf("after the take, create was called %d times, want %d: the worker's own make alone",
			got, creates+1)
	}
	for _, op := range ops {
		if got := seam.Calls(op); got != before[op] {
			t.Fatalf("after the take of spare %d, %s was called %d times, want still %d: a take makes no storage call",
				id, op, got, before[op])
		}
	}
	hold.Release()
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
	// while this spare was owed to the hold, never saw it: the refill filled
	// the target and the debt above it, so the pool holds two ready spares
	// with one still owed.
	h.Release()
	awaitCounters(t, "the replaced spare made above the target", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 && c.Held == 1 })
	if id, err := hold.Take(); err != nil || id == 0 {
		t.Fatalf("the hold's take over the replaced spare: (%d, %v)", id, err)
	}
	hold.Close()
	// The target is 1: the spare the pool holds above it was the replaced
	// demand, which the reserved take spent, so the pool settles back at its
	// target.
	awaitCounters(t, "the pool back at its target after the close", p,
		func(c segment.PoolCounters) bool { return c.Held == 0 && c.Spares == 1 })
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
	_, set2, m2 := maker(t, 0, noMax)
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
		h := &logRecorder{}
		seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, slog.New(h))
		if err := seam.Schedule(journaltest.Failing(journaltest.OpAllocate, "*", 0, syscall.EIO)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		p.Start()

		// The worker's failed make must land before the state is read: a
		// read that can race the make passing asserts nothing. The Warn the
		// failure logs is what the test waits on.
		awaitLogged(t, h, slog.LevelWarn, "did not make a spare", 1)
		if p.DeviceFull() {
			t.Fatal("the EIO set the device-full state, want it left false: only the ENOSPC of a failed make sets it")
		}
		if _, err := p.Take(segment.Content); err == nil {
			t.Fatal("a take over the faulted pool succeeded, want the spare refusal")
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

	h := &logRecorder{}
	seam, _, _, p := poolOf(t, segment.Targets{Content: 3}, 2*segCap, 0, slog.New(h))
	p.Start()
	awaitCounters(t, "the two spares the maximum holds", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	// The fill's own pass ends on the refused third make, which the worker
	// logs once at Debug: the maximum is the pool working as configured.
	// Waiting on that record is what makes the busy-loop count below
	// meaningful — a pass that never ran would draw no refusals either.
	awaitLogged(t, h, slog.LevelDebug, "did not make a spare", 1)

	// A busy pass — one that retried the refused make without a kick —
	// would log refusals in a tight loop. A bounded settle, with no kick
	// in it, catches it: the count must not grow while nothing kicked.
	before := h.countAt(slog.LevelDebug, "did not make a spare")
	for range 1 << 16 {
		runtime.Gosched()
	}
	if grown := h.countAt(slog.LevelDebug, "did not make a spare") - before; grown != 0 {
		t.Fatalf("with no kick the worker drew %d more refused-makes, want none: a pass stops on its refusal",
			grown)
	}

	// One kick draws exactly one more pass: one more refusal record, and no
	// spin behind it.
	p.Kick()
	awaitLogged(t, h, slog.LevelDebug, "did not make a spare", before+1)
	if grown := h.countAt(slog.LevelDebug, "did not make a spare") - before; grown > 4 {
		t.Fatalf("one kick drew %d refused-makes, want one pass stopping on its refusal",
			grown)
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

	// No hold parks the worker here: each class holds exactly one spare and
	// the takes are FIFO, so a refill the takes' kicks wake appends behind
	// the one spare the class already holds, and the ids below are the ones
	// the fill itself drew. A take moves the spare into the set as a
	// segment, whose id is what the test reads.
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

	// Stop is joined in parallel and h2 keeps its second make parked at the
	// seam, so Stop cannot pass wg.Wait until h2 releases below.
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

// TestPoolHoldCloseReturnsSparesToUnreservedTakes: a hold's Close returns
// the spares it still owes to unreserved takes, a spent hold takes as an
// unreserved one does, and a Hold made while the pool owes more than it
// holds reports itself short by no more than the hold it refused. It catches
// a Close that leaks its spares owed — which would refuse every later
// unreserved roll for as long as the journal is open — and a refusal that
// blames a debt larger than the hold the caller asked for.
func TestPoolHoldCloseReturnsSparesToUnreservedTakes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("Close returns the spares a reservation left unused", func(t *testing.T) {
		// The journal's maximum holds exactly the content target, so nothing
		// the takes kick can make another spare and the counts stand still.
		_, _, _, p := poolOf(t, segment.Targets{Content: 2}, 2*segCap, 0, nil)
		p.Start()
		awaitCounters(t, "the two content spares ready", p,
			func(c segment.PoolCounters) bool { return c.Spares == 2 })

		h, err := p.Hold(2)
		if err != nil {
			t.Fatalf("Hold(2) over two ready spares: %v", err)
		}
		if _, err := p.Take(segment.Content); err == nil {
			t.Fatal("an unreserved take over a two-spare hold succeeded, want the refusal")
		}

		// The reservation ends with both spares still owed: Close returns
		// them, and the unreserved take they held back is served at once.
		h.Close()
		if c := p.Counters(); c.Held != 0 {
			t.Fatalf("after the close the pool owes %d spares, want none: a reservation that ends unused must return what it held", c.Held)
		}
		if id, err := p.Take(segment.Content); err != nil || id == 0 {
			t.Fatalf("the unreserved take after the close: (%d, %v), want the returned spare", id, err)
		}

		// A second Close changes nothing.
		h.Close()
		if c := p.Counters(); c.Held != 0 {
			t.Fatalf("after the second close the pool owes %d spares, want none: Close is idempotent", c.Held)
		}
		p.Stop()
	})

	t.Run("a spent hold takes as an unreserved take does", func(t *testing.T) {
		_, _, _, p := poolOf(t, segment.Targets{Content: 3}, 3*segCap, 0, nil)
		p.Start()
		awaitCounters(t, "the three content spares ready", p,
			func(c segment.PoolCounters) bool { return c.Spares == 3 })

		h, err := p.Hold(1)
		if err != nil {
			t.Fatalf("Hold(1) over three ready spares: %v", err)
		}
		// One spare goes to an unreserved take, one to the hold itself; one
		// stays ready for the second hold below.
		if id, err := p.Take(segment.Content); err != nil || id == 0 {
			t.Fatalf("the unreserved take beside the hold: (%d, %v)", id, err)
		}
		if _, err := h.Take(); err != nil {
			t.Fatalf("the hold's own take: %v", err)
		}
		if c := p.Counters(); c.Spares != 1 || c.Held != 0 {
			t.Fatalf("after the hold spent its spare the counters are %+v, want 1 ready and none owed", c)
		}

		// The hold has spent its own spares, so its next take behaves as an
		// unreserved one. A second hold owns the one spare still ready, so
		// the spent hold is refused `spare` exactly as an unreserved take
		// over it would be — and, like every refusal, takes nothing. A spent
		// hold that took the reserved branch would steal the other hold's
		// spare and eat its debt: the counters here are what catch that.
		h2, err := p.Hold(1)
		if err != nil {
			t.Fatalf("Hold(1) over the last spare: %v", err)
		}
		_, err = h.Take()
		assertSpareRefusal(t, "the spent hold's take over another hold's spare", err, 1)
		if c := p.Counters(); c.Spares != 1 || c.Held != 1 {
			t.Fatalf("after the spent hold's refused take the counters are %+v, want 1 ready spare and 1 owed: the refusal changed nothing", c)
		}

		// Over an empty pool the spent hold is refused the same way.
		if id, err := h2.Take(); err != nil || id == 0 {
			t.Fatalf("the second hold's take: (%d, %v)", id, err)
		}
		_, err = h.Take()
		assertSpareRefusal(t, "the spent hold's take over an empty pool", err, 1)
		if c := p.Counters(); c.Spares != 0 || c.Held != 0 {
			t.Fatalf("after the spent hold's refused take the counters are %+v, want the refusal to change nothing", c)
		}
		h2.Close()

		// A replacement owed to the spent hold makes the next Hold's
		// shortage one the pool's own debt doubles: ready stands below held,
		// and the refusal still blames no more than the hold it refused.
		h.Replace()
		if _, err := p.Hold(1); err == nil {
			t.Fatal("a Hold over a pool owing more than it holds succeeded, want the refusal")
		} else {
			assertSpareRefusal(t, "the Hold the replaced debt refused", err, 1)
		}
		h.Close()
		if c := p.Counters(); c.Held != 0 {
			t.Fatalf("after the close of the replaced hold the pool owes %d spares, want none", c.Held)
		}
		p.Stop()
	})
}

// TestPoolHoldTakeAfterStopRefused: a hold opened before the pool's Stop
// takes nothing after it. Stop's contract — Take and Hold return
// core.ErrClosed — covers a reservation's takes too: closing the journal
// hands out no held spare and moves none into the set, whatever the hold
// still owes. It catches a reservation that outlives the journal's close.
func TestPoolHoldTakeAfterStopRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	_, set, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)
	p.Start()
	awaitCounters(t, "the content spare ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })

	hold, err := p.Hold(1)
	if err != nil {
		t.Fatalf("Hold(1) over the ready spare: %v", err)
	}
	p.Stop()

	if _, err := hold.Take(); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the held take after Stop: %v, want core.ErrClosed", err)
	}
	if c := p.Counters(); c.Held != 1 {
		t.Fatalf("after the refused take the pool owes %d spares, want the hold's 1 untouched", c.Held)
	}

	// Close is still safe after Stop, and returns what the hold owes.
	hold.Close()
	if c := p.Counters(); c.Held != 0 {
		t.Fatalf("after the close the pool owes %d spares, want none", c.Held)
	}
	l := set.Counters()
	if l.Segments != 0 {
		t.Fatalf("after Stop the ledger holds %d segments, want none: the shutting-down journal handed out no spare", l.Segments)
	}
	if err := set.Check(); err != nil {
		t.Fatalf("after Stop: %v", err)
	}
}

// TestPoolDeviceFullOnlyAMadeSpareClears: the state a roll reads as a full
// device is set by the ENOSPC of a failed make and cleared only by a make
// that succeeds — a later failure of any other kind does not clear it,
// however it fails, for no spare has been made since the device filled. It
// catches a state cleared by a wrong error while the device is still full.
func TestPoolDeviceFullOnlyAMadeSpareClears(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)

	// The device fills: every allocation fails ENOSPC.
	if err := seam.Schedule(journaltest.Failing(journaltest.OpAllocate, "*", 0, syscall.ENOSPC)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	p.Start()
	awaitDeviceFull(t, true, p)

	// The device still fails, now with an ordinary fault. No spare has been
	// made: the state a roll reads must stand.
	if err := seam.Schedule(journaltest.Failing(journaltest.OpAllocate, "*", 0, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	// The second failure must land before the state is read: the count of
	// allocations — two, one per failed make — is the arrival of both.
	p.Kick()
	for seam.Calls(journaltest.OpAllocate) < 2 {
		runtime.Gosched()
	}
	awaitDeviceFull(t, true, p)
	if !p.DeviceFull() {
		t.Fatal("the EIO after the ENOSPC cleared the device-full state, want it to stand until a spare is made")
	}

	// The fault clears and a spare is made: only now does the state go.
	if err := seam.Schedule(); err != nil {
		t.Fatalf("the cleared Schedule: %v", err)
	}
	p.Kick()
	awaitCounters(t, "the spare made over the cleared fault", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })
	awaitDeviceFull(t, false, p)
	p.Stop()
}

// TestPoolSpentHoldTakesAnUnowedSpare: a spent hold takes exactly as an
// unreserved take does, including succeeding: over a spare no hold owes, the
// spent hold returns it and the counters drop as an unreserved take's do. It
// catches a spent hold that always refuses, whose reservation would lose the
// spares the unreserved takes around it are free to take.
func TestPoolSpentHoldTakesAnUnowedSpare(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The journal's maximum holds exactly the content target, so the counts
	// below stand on the takes alone and no refill races them.
	_, set, _, p := poolOf(t, segment.Targets{Content: 2}, 2*segCap, 0, nil)
	p.Start()
	awaitCounters(t, "the two content spares ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	h, err := p.Hold(1)
	if err != nil {
		t.Fatalf("Hold(1) over two ready spares: %v", err)
	}
	// The hold spends its own spare; one ready spare remains, owed to no one.
	if id, err := h.Take(); err != nil || id == 0 {
		t.Fatalf("the hold's own take: (%d, %v)", id, err)
	}
	if c := p.Counters(); c.Spares != 1 || c.Held != 0 {
		t.Fatalf("after the hold spent its spare the counters are %+v, want 1 ready and none owed", c)
	}

	// The spent hold takes the unowed spare as an unreserved take would.
	id, err := h.Take()
	if err != nil || id == 0 {
		t.Fatalf("the spent hold's take over an unowed spare: (%d, %v), want the spare", id, err)
	}
	if c := p.Counters(); c.Spares != 0 || c.Held != 0 {
		t.Fatalf("after the spent hold's take the counters are %+v, want the pool empty", c)
	}
	if c := set.Counters(); c.Segments != 2 || c.Spares != 0 {
		t.Fatalf("after both takes the ledger holds %d segments and %d spares, want 2 and 0", c.Segments, c.Spares)
	}
	if err := set.Check(); err != nil {
		t.Fatalf("after the takes: %v", err)
	}
	h.Close()
	p.Stop()
}

// TestPoolHoldAboveTheTargetIsSatisfiableOnRetry: a reservation asking for
// more spares than the content target is refused `spare` once and served on
// the retry, because `spare` is the one refusal that clears on its own — the
// pool makes up the shortfall off the write path rather than answering a
// request the journal can serve with a refusal that never clears. It catches
// a hold above the target that is refused forever.
func TestPoolHoldAboveTheTargetIsSatisfiableOnRetry(t *testing.T) {
	prof.Watch(t, prof.Options{})

	_, set, _, p := poolOf(t, segment.Targets{Content: 2}, noMax, 0, nil)
	p.Start()
	awaitCounters(t, "the target's two spares ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	// The first ask falls one spare short and is refused with what it lacked.
	_, err := p.Hold(3)
	assertSpareRefusal(t, "the Hold(3) above the target", err, 1)

	// The refused ask is demand the refill makes up: the retry must find the
	// third spare. The retry is the engine's own — refused, then asked again
	// once the pool has made the spare up — so it is bounded-yielded here.
	var h *segment.Hold
	for range poolAttempts {
		var herr error
		h, herr = p.Hold(3)
		if herr == nil {
			break
		}
		assertSpareRefusal(t, "a retried Hold(3) while the shortfall is made up", herr, 1)
		runtime.Gosched()
	}
	if h == nil {
		t.Fatalf("the retried Hold(3) never cleared: the pool made up no shortfall the refusal asked for")
	}
	if c := p.Counters(); c.Held != 3 {
		t.Fatalf("after the retried hold the pool owes %d spares, want the hold's 3", c.Held)
	}

	// The hold's three takes are served, and its close returns the pool to
	// its target's shape.
	for i := 1; i <= 3; i++ {
		if id, err := h.Take(); err != nil || id == 0 {
			t.Fatalf("the hold's take %d: (%d, %v)", i, id, err)
		}
	}
	h.Close()
	awaitCounters(t, "the pool back at its target after the close", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 && c.Held == 0 })
	if err := set.Check(); err != nil {
		t.Fatalf("after the holds: %v", err)
	}
	p.Stop()
}

// TestPoolOpenHoldDoesNotStarveUnreservedRolls: a hold's spares ride above
// the content target rather than inside it, so an open reservation does not
// consume the spares the target exists to keep: every stream can still roll
// once without waiting for a spare. It catches a target that a hold empties,
// whose unreserved rolls would be refused for the life of the reservation.
func TestPoolOpenHoldDoesNotStarveUnreservedRolls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	_, set, _, p := poolOf(t, segment.Targets{Content: 2}, noMax, 0, nil)
	p.Start()
	awaitCounters(t, "the target's two spares ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	h, err := p.Hold(2)
	if err != nil {
		t.Fatalf("Hold(2) over two ready spares: %v", err)
	}
	defer h.Close()

	// The take right after the hold may race the refill that fills above the
	// held pair; the bounded wait is the retry the engine performs, and the
	// take must be served while the pool can make.
	var id core.SegmentID
	for range poolAttempts {
		id, err = p.Take(segment.Content)
		if err == nil {
			break
		}
		assertSpareRefusal(t, "a refused unreserved take over an open hold", err, 1)
		runtime.Gosched()
	}
	if err != nil || id == 0 {
		t.Fatalf("the unreserved take over an open hold: (%d, %v), want served: the target keeps spares above what the hold owes", id, err)
	}
	if err := set.Check(); err != nil {
		t.Fatalf("after the take: %v", err)
	}
	p.Stop()
}

// TestPoolReservedTakeServesAReadySpareItOwesMoreThan: a reserved take is
// refused only when no spare is ready, never because more are owed to holds
// — the spares a reservation holds are its own demand, and R80's "a reserved
// roll takes one of them" is served over the one ready spare whatever the
// debt above it. It catches a reserved take that demands spares it was never
// owed, which would starve the very reservation Replace stands for.
func TestPoolReservedTakeServesAReadySpareItOwesMoreThan(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The journal's maximum is pinned at the fill, so no make can raise the
	// ready count and the debt stands for the test to read.
	_, set, _, p := poolOf(t, segment.Targets{Content: 2}, 2*segCap, 0, nil)
	p.Start()
	awaitCounters(t, "the two content spares ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	h1, err := p.Hold(1)
	if err != nil {
		t.Fatalf("the first Hold(1): %v", err)
	}
	h2, err := p.Hold(1)
	if err != nil {
		t.Fatalf("the second Hold(1): %v", err)
	}
	// The first hold takes its spare, then its write is rolled short by
	// interleaving and the reservation takes a replacement: the pool now
	// owes 2 with one spare ready.
	if _, err := h1.Take(); err != nil {
		t.Fatalf("the first hold's take: %v", err)
	}
	h1.Replace()
	if c := p.Counters(); c.Spares != 1 || c.Held != 2 {
		t.Fatalf("after the replace the counters are %+v, want 1 ready and 2 owed", c)
	}

	// The second hold's reserved take is served over the one ready spare,
	// although the pool owes more than it holds.
	if id, err := h2.Take(); err != nil || id == 0 {
		t.Fatalf("the reserved take over the one ready spare: (%d, %v), want served", id, err)
	}
	if c := p.Counters(); c.Spares != 0 || c.Held != 1 {
		t.Fatalf("after the reserved take the counters are %+v, want none ready and the replaced hold's 1 owed", c)
	}
	if err := set.Check(); err != nil {
		t.Fatalf("after the takes: %v", err)
	}
	h1.Close()
	h2.Close()
	p.Stop()
}

// TestPoolReplaceAloneWakesTheRefill: a replacement's own kick is what wakes
// the worker — the spares a replace re-owes the reservation are demand the
// refill makes up, and nothing else is in flight by then. It catches a
// Replace that leaves its demand on a kick some earlier take may have left
// buffered, whose reserved write would then wait on a wake that never comes.
func TestPoolReplaceAloneWakesTheRefill(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)
	p.Start()
	awaitCounters(t, "the first spare ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 && seam.Calls(journaltest.OpCreate) == 1 })

	h, err := p.Hold(1)
	if err != nil {
		t.Fatalf("Hold(1) over the ready spare: %v", err)
	}
	if _, err := h.Take(); err != nil {
		t.Fatalf("the hold's take: %v", err)
	}
	// The take's own refilled spare must land, and then the worker must be
	// calm: nothing short, no kicks buffered. A settle with no makes is what
	// makes the replace below the only wake in flight.
	awaitCounters(t, "the take's refill landed", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 && seam.Calls(journaltest.OpCreate) == 2 })
	made := seam.Calls(journaltest.OpCreate)
	for range 1 << 14 {
		if seam.Calls(journaltest.OpCreate) != made {
			t.Fatalf("the pool made a file while nothing was short, want the settle calm")
		}
		runtime.Gosched()
	}

	// The replace alone re-owes a spare the pool holds no spare for: its own
	// kick is what wakes the worker to make it. The wait's condition is the
	// pool's own settled state — the make landed in the class, not merely
	// begun at the seam — with the create count risen beside it, so nothing
	// after the wait can still race the make in flight.
	h.Replace()
	awaitCounters(t, "the replaced demand made and landed", p,
		func(c segment.PoolCounters) bool {
			return c.Spares == 2 && c.Held == 1 && seam.Calls(journaltest.OpCreate) == made+1
		})
	h.Close()
	p.Stop()
}

// TestPoolTakeServesTheOldestSpareFirst: the ready list is FIFO, so a class's
// takes hand out the spares in the order the worker made them — the oldest
// first. It catches a pop from the wrong end, whose youngest spare would be
// handed out first and the oldest left to age behind every burst.
func TestPoolTakeServesTheOldestSpareFirst(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The journal's maximum is pinned at the fill, so the two spares of the
	// target are all the pool will ever hold and their order is the order
	// the worker made them in, ascending ids.
	_, _, _, p := poolOf(t, segment.Targets{Content: 2}, 2*segCap, 0, nil)
	p.Start()
	awaitCounters(t, "the two content spares ready", p,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	first, err := p.Take(segment.Content)
	if err != nil || first == 0 {
		t.Fatalf("the first take: (%d, %v)", first, err)
	}
	second, err := p.Take(segment.Content)
	if err != nil || second == 0 {
		t.Fatalf("the second take: (%d, %v)", second, err)
	}
	if first >= second {
		t.Fatalf("the takes handed out %d then %d, want the ids ascending: the oldest spare first",
			first, second)
	}
	p.Stop()
}
