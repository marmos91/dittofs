//go:build journal_conformance

package conformance

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal/journaltest"
)

// The mixed-durability protocol against fakes: the proof half of the rebuilt
// check. runMixedRounds is the committed code the check runs against a
// journal; here it runs against fake journals whose designs answer the ways
// RFC 1 §5.2 permits — a correct skip-fsync design, a correct always-fsync
// design, the starving form, designs that roll, with and without a name
// change, a slow one whose writes and fsyncs take real time, a group-commit
// one whose syncs join whatever fsync is in flight — and against designs
// that are wrong on purpose: one that fails its writes, one that never syncs
// while reporting success, one that renames on every append. The correct
// designs pass at the bound; the wrong ones fail the run. The cases drive
// the protocol itself — the stream goroutine, the feeds, the teardown — so a
// protocol bug fails here first, against fakes whose every state it can see.
//
// Some cases run at the check's own interval counts — 64 and 256 intervals,
// fifteen to a segment — with a watcher that counts every closing file-end
// sync that ran while the gate was armed: the boundary rule must keep all of
// them unarmed, and a rule keyed on anything but the stream's real position
// fails here loudly. One case reads the floor from plain fields the stream
// goroutine advances, the shape of the real fixture env before it was
// guarded: under -race it proves the protocol never reads the env's state
// while the stream is mid-append.
//
// No case touches the fixture, so the suite's known failures — every journal
// test fails at "fixture: init: journal: not implemented" — do not own these:
// they run here and now, and pass.

// fakeRecordBytes is one record's on-segment bytes for n bytes of payload:
// padded and headed as the journal writes it, the same arithmetic the
// check's bound uses, at the fakes' own scale.
func fakeRecordBytes(n int64) int64 { return int64(96+8+n+7) &^ 7 }

// The fakes' stream: thirteen intervals in two files of five and eight, so
// a file end lands mid-run and the boundary rule skips those rounds, and so
// small that a run takes microseconds where the check's takes the row's own
// 64 and 256 MiB.
//
// fakeSlowWrite, fakeSlowFsync and fakeSlowPostRoll are the slow design's
// latencies: the write lands a little after Append is entered, the fsync
// takes real time after its park, and a rolled write's record lands well
// after its segment's seal sync — together they make a fed write in flight
// when the window closes if the protocol does not wait for it.
const (
	fakeIntervalBytes = int64(1 << 16)
	fakeSmallBytes    = int64(4096)

	fakeSlowWrite    = 500 * time.Microsecond
	fakeSlowFsync    = time.Millisecond
	fakeSlowPostRoll = 5 * time.Millisecond
)

// fakeSegCap is the fakes' segment capacity: three stream intervals. A
// capacity-driven roll lands mid-run without the protocol being told, and
// leaves each active segment with room for the intervals the window feeds
// beside the small record.
var fakeSegCap = fakeRecordBytes(fakeIntervalBytes) * 3

var fakeFiles = []int{5, 8}

// fakeCheckFiles is the check's own interval counts: the two files of 64 and
// 256 one-MiB intervals, at the fakes' scale.
var fakeCheckFiles = []int{64, 256}

// fakeCheckSegCap is the fakes' capacity at the check's counts: fifteen
// intervals per segment, writeRecordBytes(1 MiB) into a 16 MiB one.
var fakeCheckSegCap = fakeRecordBytes(fakeIntervalBytes) * 15

// fakeHeaderBytes is the header of a segment a roll takes, made durable by
// the first sync that covers the segment afterwards: the check's own
// segmentHeaderBytes, at any scale.
const fakeHeaderBytes = int64(48)

// mixedDesign names the sync behaviour a fake journal implements.
type mixedDesign string

const (
	// designSkip fsyncs only when the small writer's own record is pending
	// — and never on the stream's own syncs alone: the correct design that
	// skips what does not need it.
	designSkip mixedDesign = "skip"

	// designAlways fsyncs on every Sync call, whatever is pending: the
	// correct design that never skips.
	designAlways mixedDesign = "always"

	// designStarve fsyncs in a loop until nothing of the segment is dirty:
	// the natural starving form, the one a backlog could grow.
	designStarve mixedDesign = "starve"

	// designRollNoName rolls on the write path when an interval no longer
	// fits, taking a spare the directory already holds, RFC 28 §2.2's way:
	// the name set does not change, so a roll landing inside a window keeps
	// the window measured — and inside the bound.
	designRollNoName mixedDesign = "roll without a name change"

	// designRollRename rolls the same capacity-driven, write-path way but
	// renames a fresh spare into place: the name set changes, and the
	// windows a roll lands in skip the bound, as the check says they must.
	designRollRename mixedDesign = "roll renaming spares"

	// designSlowRoll fsyncs on a write path that takes real time: the write
	// lands after Append is entered, the fsync takes real time after its
	// park, and a rolled write's record lands well after its seal sync. It
	// is correct just as always is; what it proves is that the protocol
	// waits for a fed write's confirmation before the next round reads the
	// stream's state.
	designSlowRoll mixedDesign = "slow roll"

	// designCoalesce is group commit: a sync that finds another fsync in
	// flight waits for it and then issues its own, so the closing sync of
	// a finished file joins whatever measured fsync is parked rather than
	// parking behind it. Correct — and the design that stalls worst when
	// the boundary rule arms a round the stream's file-end sync lands in.
	designCoalesce mixedDesign = "group commit"

	// designNeverSyncs returns success from every sync and flushes nothing.
	designNeverSyncs mixedDesign = "never syncs"

	// designFailsWrite makes the stream's third write fail.
	designFailsWrite mixedDesign = "failing write"

	// designFailsSync makes the small writer's first measured Sync fail.
	designFailsSync mixedDesign = "failing sync"
)

// fakeCase is one choreography case: the design the fake journal answers
// with, and the knobs that vary how the protocol drives it.
type fakeCase struct {
	design mixedDesign

	// files is the interval count of each streamed file; the fakes' default
	// when nil.
	files []int

	// segCap is the record-byte capacity of one segment; the fakes' default
	// when zero.
	segCap int64

	// renameEvery rolls and renames a fresh spare into place on every
	// append, so every window's name set changes: the vacuity guard's
	// wrong design.
	renameEvery bool

	// racyFloor reads the floor from plain fields the stream goroutine
	// advances on no lock at all — the shape of the real fixture env
	// before it was guarded. The always-on pin is the floor assertion: a
	// protocol that reads the env's state while the stream is mid-append
	// fails it on every run. The race detector is a second guard, schedule
	// permitting: the out-of-lock pair reports DATA RACE wherever a run
	// actually overlaps them.
	racyFloor bool

	// watchClosing counts every closing file-end sync that ran while the
	// gate was armed: a correct boundary rule keeps that count at zero.
	watchClosing bool
}

// fakeMixed is a fake journal for runMixedRounds. It models what the check
// can see of a real one through the storage seam: one stream of records
// into an active segment — small records beside the stream's — syncs that
// flush what is pending, rolls that seal the old segment and take a fresh
// one, and a gate whose armed turns park exactly the segment fsyncs.
//
// Nothing here knows what the protocol will ask: a design is checked the
// same way the real journal will be, through Install, Append, SyncStream,
// WriteSmall, SyncSmall and the interest of the seam — durable totals and
// segment names — alone.
type fakeMixed struct {
	design mixedDesign
	gate   *journaltest.SimGate
	opts   fakeCase

	mu sync.Mutex

	// The stream's position: the file it is in, the intervals it has landed
	// in all, and the record bytes of them.
	file       int
	intervals  int
	landed     int64
	files      []int
	segCap     int64  // record-byte capacity of the active segment, resolved
	filled     int64  // record bytes physically in the active segment, flushed or not
	pending    int64  // write bytes in the active segment, not flushed
	active     string // the active segment's name
	durable    int64  // write bytes flushed in all
	names      []string
	takenCount int // segments taken by rolls
	smalls     int // small records the protocol wrote
	smallPend  int // small records not yet flushed, for the skip design's rule

	// inFlight is how many fsyncs are inside their park or their latency
	// window right now; a coalescing sync waits for it to drain.
	inFlight     int
	inFlightCond *sync.Cond

	// The racy-floor shape of the real fixture env: plain fields the
	// stream goroutine advances on no lock (a defer past mu) and
	// DurableFloor reads without one when the case asks for it. The
	// protocol's quiescence — the fed-wait, the confirmation waits — is
	// the only thing that keeps the pair from ever overlapping; when it
	// fails, the floor assertion fails every run and the detector reports
	// the overlaps a schedule exposes.
	floorIntervals int
	floorSmalls    int

	// closingArmed counts the closing file-end syncs that ran while the
	// gate was armed. It is written on the stream goroutine and read after
	// the run joined the stream, so atomic is enough.
	closingArmed atomic.Int64
}

// newFakeCase builds one case's fake journal.
func newFakeCase(c fakeCase) *fakeMixed {
	files := fakeFiles
	if c.files != nil {
		files = c.files
	}
	segCap := fakeSegCap
	if c.segCap > 0 {
		segCap = c.segCap
	}
	f := &fakeMixed{
		design: c.design,
		opts:   c,
		files:  slices.Clone(files),
		segCap: segCap,
		active: "seg-0.seg",
		names:  []string{"seg-0.seg"},
	}
	f.inFlightCond = sync.NewCond(&f.mu)
	return f
}

// Install takes the gate the protocol parks its syncs at, as the seam's
// fault does for a real journal.
func (f *fakeMixed) Install(gate *journaltest.SimGate) {
	f.gate = gate
}

// fsync parks at the gate — where it is armed, as the journal's fsync parks
// at storage — and takes real time doing the work after the park when the
// design is slow: the write the sync covers is not the whole segment's
// state, only what was in it when the sync started. The call is counted in
// flight from its park to its finish, so a coalescing sync can find it.
// Callers hold mu on entry; mu is released around the park, because a
// parked call is a goroutine the gate must free.
func (f *fakeMixed) fsync(name string) {
	f.inFlight++
	f.mu.Unlock()
	f.gate.At(journaltest.OpSync, []byte(name))
	if f.design == designSlowRoll {
		time.Sleep(fakeSlowFsync)
	}
	f.mu.Lock()
	f.inFlight--
	f.inFlightCond.Broadcast()
}

// joinInFlight waits for every fsync in flight to finish, as a group-commit
// sync does before issuing its own. The caller holds mu.
func (f *fakeMixed) joinInFlight() {
	for f.inFlight > 0 {
		f.inFlightCond.Wait()
	}
}

// flush makes the active segment's pending bytes durable, as a sync that
// returns success does, and settles the small writer's pending with it. The
// caller holds mu.
func (f *fakeMixed) flush() {
	f.durable += f.pending
	f.pending = 0
	f.smallPend = 0
}

// take rolls to a fresh segment: the old one is sealed — gated, as a rolling
// write path's seal sync is — and the new one takes a header write. When
// rename is set the fresh name joins the set, as a journal renaming a spare
// into place does; RFC 28 §2.2's roll from an existing spare joins nothing.
// The caller holds mu on entry and keeps holding it through.
func (f *fakeMixed) take(rename bool) {
	f.fsync(f.active)
	if f.design != designNeverSyncs {
		f.flush()
	}
	f.takenCount++
	f.active = "seg-" + strconv.Itoa(f.takenCount) + ".seg"
	if rename {
		f.names = append(f.names, f.active)
	}
	f.pending += fakeHeaderBytes
	f.filled = fakeHeaderBytes
}

// Append lands one interval of the stream, rolling the segment when the
// interval will not fit, and runs the closing sync when the interval ends a
// file. It is the stream goroutine the protocol drives.
func (f *fakeMixed) Append() (more bool, err error) {
	if f.design == designSlowRoll {
		time.Sleep(fakeSlowWrite)
	}
	f.mu.Lock()
	// The plain-field floor variant's snapshot, taken under the lock and
	// published outside it below — after Append returns, by the defer the
	// outer mu.Unlock cannot reach — so the fake's writers advance the two
	// plain fields on no lock at all, the old fixture env's shape.
	smallSnapshot := f.smalls
	if f.opts.racyFloor {
		defer func() {
			f.floorIntervals++
			f.floorSmalls = smallSnapshot
		}()
	}
	defer f.mu.Unlock()

	if f.design == designFailsWrite && f.intervals == 2 {
		return false, errors.New("the fake stream's write failed")
	}

	f.intervals++
	f.landed += fakeRecordBytes(fakeIntervalBytes)
	roll := f.opts.renameEvery || (f.filled+fakeRecordBytes(fakeIntervalBytes) > f.segCap && f.design != designNeverSyncs)
	if roll {
		f.take(f.opts.renameEvery || f.design == designRollRename)
		if f.design == designSlowRoll {
			// The rolled write's record lands well after its seal sync:
			// with mu released, a freed measured sync can flush without
			// covering it, and its confirmation is late.
			f.mu.Unlock()
			time.Sleep(fakeSlowPostRoll)
			f.mu.Lock()
		}
	}
	f.pending += fakeRecordBytes(fakeIntervalBytes)
	f.filled += fakeRecordBytes(fakeIntervalBytes)

	// The closing sync of a finished file: no design that answers its Sync
	// calls may return leaving it unsynced.
	if f.intervalsInFile() == f.files[f.file] {
		if f.opts.watchClosing && f.gate.Armed() {
			f.closingArmed.Add(1)
		}
		if f.design == designCoalesce {
			f.joinInFlight()
		}
		f.fsync(f.active)
		if f.design != designNeverSyncs {
			f.flush()
		}
		f.file++
		if f.file >= len(f.files) {
			return false, nil
		}
	}
	return true, nil
}

// intervalsInFile is how many of the stream's intervals the file it is in
// holds.
func (f *fakeMixed) intervalsInFile() int {
	n := f.intervals
	for _, size := range f.files[:f.file] {
		n -= size
	}
	return n
}

// EndsFile reports whether the interval of index i is the last of its file.
func (f *fakeMixed) EndsFile(i int) bool {
	n := 0
	for _, size := range f.files {
		n += size
		if i == n-1 {
			return true
		}
	}
	return false
}

// Intervals is how many intervals the fakes' files hold.
func (f *fakeMixed) Intervals() int {
	total := 0
	for _, size := range f.files {
		total += size
	}
	return total
}

// SyncStream runs the journal's own sync of the streamed file on the run's
// goroutine. Every design that answers its Sync calls flushes the segment;
// the protocol runs it with the gate disarmed, so its fsync parks nowhere,
// exactly as the check's plain sync runs through a disarmed seam. It is not
// where a sync-rolling design rolls: the protocol reads SyncStream as the
// stream's own plain sync, the same call the journal's stream writer makes
// of itself, and a roll here would make the round's opening interval land on
// a fresh segment every time. Rolls belong to the write path, where the
// interval no longer fits, and to the small writer's sync below.
//
// The slow design's SyncStream covers what was pending when it started: a
// sync that takes real time cannot cover what lands after it began, so a
// write that sneaks in during one — what a desynchronized round does — is
// caught by the floor, as it would be in a real journal whose fsync raced a
// late append.
func (f *fakeMixed) SyncStream() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.design == designNeverSyncs {
		return nil
	}
	if f.design == designCoalesce {
		f.joinInFlight()
	}
	if f.design == designSlowRoll {
		covered, coveredSmalls := f.pending, f.smallPend
		f.fsync(f.active)
		f.durable += covered
		f.pending -= covered
		if coveredSmalls > 0 {
			f.smallPend = 0
		}
		return nil
	}
	f.fsync(f.active)
	f.flush()
	return nil
}

// WriteSmall lands the small writer's record beside the stream's. It may run
// while the stream goroutine is mid-append in a run whose protocol does not
// quiesce them; the fields it touches share the fake's lock, and the racy
// floor variant reads the position it advanced from without one.
func (f *fakeMixed) WriteSmall() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.smalls++
	f.smallPend++
	f.pending += fakeRecordBytes(fakeSmallBytes)
	return nil
}

// SyncSmall runs the small writer's measured Sync the way its design does.
// It runs on its own goroutine, as the check's measured Sync does.
func (f *fakeMixed) SyncSmall() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch f.design {
	case designNeverSyncs:
		// Success reported, nothing flushed.
		return nil
	case designFailsSync:
		if f.smalls == 1 {
			return errors.New("the fake journal's Sync failed")
		}
		fallthrough
	case designSkip:
		// The skip design fsyncs only while its own record is pending. In
		// the measured window one always is: the protocol wrote it just
		// before arming, after the plain sync emptied the segment.
		if f.smallPend == 0 {
			return nil
		}
	}

	switch f.design {
	case designStarve:
		// Flush while anything of the segment is dirty. Each pass parks at
		// the gate, so each is fed one interval and covers it; what the
		// starving loop cannot do under the gated feeder is outgrow one
		// interval per ask.
		for f.pending > 0 || f.smallPend > 0 {
			f.fsync(f.active)
			f.flush()
		}
		return nil
	}

	// skip, always, the roll ones and group commit: flushing designs answer
	// one fsync with one flush. The roll designs roll on the write path,
	// where the interval no longer fits, exactly as a capacity-bound
	// journal does, so a roll inside a window is a fed write's own and not
	// this sync's; the coalescing one joins whatever fsync is in flight
	// first, which inside a window is exactly nobody, because its gate
	// keeps the writes fed one at a time.
	if f.design == designCoalesce {
		f.joinInFlight()
	}
	f.fsync(f.active)
	f.flush()
	return nil
}

// DurableTotal is the fakes' durable write bytes across all segments.
func (f *fakeMixed) DurableTotal() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.durable
}

// SegmentNames lists the segment files the fake journal's directory holds.
func (f *fakeMixed) SegmentNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.names)
}

// WindowBound is what one window may make durable at the fakes' scale: the
// small record, one fed interval, and a roll's header.
func (f *fakeMixed) WindowBound() int64 {
	return fakeHeaderBytes + fakeRecordBytes(fakeIntervalBytes) + fakeRecordBytes(fakeSmallBytes)
}

// DurableFloor is what everything written so far must have made durable
// once a sync has returned success on it: the stream's intervals and the
// small records, as record bytes. Headers flush beside the records and are
// left out, so a design that rolled more often stays inside the floor.
//
// The racy case reads this from the plain fields the stream goroutine
// advances, without the lock: the real fixture env had exactly that shape
// before it was guarded, and the protocol owes every env the quiescence
// that keeps the pair from overlapping.
func (f *fakeMixed) DurableFloor() int64 {
	if f.opts.racyFloor {
		return int64(f.floorIntervals)*fakeRecordBytes(fakeIntervalBytes) + int64(f.floorSmalls)*fakeRecordBytes(fakeSmallBytes)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.landed + int64(f.smalls)*fakeRecordBytes(fakeSmallBytes)
}

// ClosingArmedCount is how many closing file-end syncs ran while the gate
// was armed. It is read after the run joined the stream goroutine.
func (f *fakeMixed) ClosingArmedCount() int64 {
	return f.closingArmed.Load()
}

// catchFatalf is the sentinel a caught failure unwinds on.
var catchFatalf = errors.New("caught Fatalf")

// caughtRun is testing.T-shaped recording TB: Fatalf and Errorf record and
// unwind, and the cleanups the protocol registers run after, so the run's
// goroutines are torn down even when the case ends halfway. It is the
// choreography's way to assert that a wrong design fails the run without
// failing the test that asserts it.
type caughtRun struct {
	cleanups []func()
	failures []string
}

func (c *caughtRun) Helper()           {}
func (c *caughtRun) Cleanup(fn func()) { c.cleanups = append(c.cleanups, fn) }
func (c *caughtRun) Fatalf(format string, a ...any) {
	c.failures = append(c.failures, "fatal: "+fmt.Sprintf(format, a...))
	panic(catchFatalf)
}
func (c *caughtRun) Errorf(format string, a ...any) {
	c.failures = append(c.failures, "error: "+fmt.Sprintf(format, a...))
}

// runCleanups runs the registered cleanups in reverse, as testing does, in a
// defer-driven way: the runs end through them even when caught.
func (c *caughtRun) runCleanups() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}

// checkEnvClean asserts the manners a finished run owes: nothing parked, no
// arrival dropped, and — for a watching case — no closing file-end sync
// that ran with the gate armed.
func checkEnvClean(t *testing.T, env *fakeMixed) {
	t.Helper()
	if env.gate.Parked() != 0 {
		t.Fatalf("the run left %d gate calls parked, freed by nothing", env.gate.Parked())
	}
	if env.gate.Dropped() != 0 {
		t.Fatalf("the run lost %d arrivals the protocol never drained", env.gate.Dropped())
	}
	if env.opts.watchClosing && env.ClosingArmedCount() != 0 {
		t.Fatalf("%d closing file-end syncs ran with the gate armed: the boundary rule disarmed no round that needed it, and the stream's own sync met the gate", env.ClosingArmedCount())
	}
}

// TestCounted_MixedChoreography drives runMixedRounds against fakes. The
// correct designs pass at the bound; the wrong ones fail, so the protocol's
// checks — the window's bound, the floor settled syncs, the guard against a
// vacuous run, the quiescence of the stream between a window and the next
// round, the teardown with nothing parked — hold against designs that
// answer like journals do, wherever the fixture cannot.
//
// The cases at the check's counts watch the closing syncs: the boundary
// rule must keep everyone of them off the armed gate, or a correct
// journal's own file-end sync parks where a wrong protocol would feed it
// an interval it cannot take.
func TestCounted_MixedChoreography(t *testing.T) {
	correct := []fakeCase{
		{design: designSkip},
		{design: designAlways},
		{design: designStarve},
		{design: designRollNoName},
		{design: designRollRename},
		{design: designCoalesce},
		{design: designSlowRoll},
	}
	for _, tc := range correct {
		t.Run(string(tc.design)+" passes", func(t *testing.T) {
			env := newFakeCase(tc)
			runMixedRounds(t, env)
			checkEnvClean(t, env)
		})
	}

	// The check's own interval counts, with the watcher on: the boundary
	// rule reads the stream's real position here, where a rule keyed on
	// anything else drifts into arming a round whose interval ends a file.
	// The slow roll exercised here is the one whose freed fed write
	// outlives its window — the roll-seal sync parks behind the interval
	// the protocol hands it, and its record lands after the seal — and
	// group commit is the one that stalls worst when a closing sync meets
	// the armed gate.
	atCheckCounts := []fakeCase{
		{design: designAlways, files: fakeCheckFiles, segCap: fakeCheckSegCap, watchClosing: true},
		{design: designCoalesce, files: fakeCheckFiles, segCap: fakeCheckSegCap, watchClosing: true},
		{design: designSlowRoll, files: fakeCheckFiles, segCap: fakeCheckSegCap, watchClosing: true},
	}
	for _, tc := range atCheckCounts {
		t.Run(string(tc.design)+" at the check's counts passes", func(t *testing.T) {
			env := newFakeCase(tc)
			runMixedRounds(t, env)
			checkEnvClean(t, env)
		})
	}

	// The fixture env's old shape: the floor read from plain fields the
	// stream goroutine advances on no lock, at the check's own counts
	// where a freed fed write actually outlives its window. The floor
	// assertion is the always-on pin — a protocol that reads mid-append
	// fails it on every run — and the race detector is the second guard,
	// reporting the out-of-lock pair wherever a schedule overlaps it.
	t.Run("slow roll over plain position fields passes", func(t *testing.T) {
		env := newFakeCase(fakeCase{design: designSlowRoll, racyFloor: true, files: fakeCheckFiles, segCap: fakeCheckSegCap})
		runMixedRounds(t, env)
		checkEnvClean(t, env)
	})

	wrong := []struct {
		name string
		opts fakeCase
		want string
	}{
		{"never syncs", fakeCase{design: designNeverSyncs}, "less durable than written"},
		{"failing write", fakeCase{design: designFailsWrite}, "opening write"},
		{"failing sync", fakeCase{design: designFailsSync}, "measured Sync"},
		// Names change on every append, so every window is skipped and the
		// run measures nothing: the vacuity guard is the only thing that
		// fails it, and this case is the pin that disabling the guard
		// cannot pass.
		{"roll renaming on every append", fakeCase{design: designAlways, renameEvery: true}, "the check proved nothing"},
	}
	for _, tc := range wrong {
		t.Run(tc.name+" fails", func(t *testing.T) {
			caught := &caughtRun{}
			func() {
				defer func() {
					if r := recover(); r != nil {
						if err, ok := r.(error); !ok || !errors.Is(err, catchFatalf) {
							panic(r)
						}
					}
				}()
				defer caught.runCleanups()
				runMixedRounds(caught, newFakeCase(tc.opts))
			}()
			if len(caught.failures) == 0 {
				t.Fatalf("the wrong design %s passed the run: the protocol failed it nowhere", tc.name)
			}
			if !strings.Contains(caught.failures[0], tc.want) {
				t.Fatalf("the wrong design %s failed on %q, want a failure naming %q", tc.name, caught.failures[0], tc.want)
			}
		})
	}
}
