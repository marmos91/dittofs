package journalv2test

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"github.com/marmos91/dittofs/pkg/sim"
)

// The fixture's journal settings.
const (
	// FixtureSegmentCap is the segment size the fixture's journals use, so a
	// check over many segments fits in a temporary directory.
	FixtureSegmentCap = 1 << 20

	// FixtureMaxBytes is the fixture's journal maximum: small enough that a
	// check can drive the journal to capacity, and a whole number of segments.
	FixtureMaxBytes = 64 * FixtureSegmentCap

	// fixtureStreams is the fixture's append streams: two, the fewest on which
	// two files can be written concurrently through different streams.
	fixtureStreams = 2

	// fixtureHeadroomRecords is the fixture's headroom for records without
	// bytes, set so the share of FixtureMaxBytes it takes is small and fixed
	// rather than the default's.
	fixtureHeadroomRecords = 1024

	// FixtureIdleSealBytes is how full an idle segment must be for the
	// fixture's journals to seal it: the default's sixteenth of a segment, at
	// the fixture's cap, so idle sealing can happen at all. A journal with
	// another cap, such as ModelTargets opens, takes the sixteenth of its own.
	FixtureIdleSealBytes = FixtureSegmentCap / 16

	// IdleSealAge is how long a segment takes no append before the journal
	// seals it, if it is at least FixtureIdleSealBytes full.
	IdleSealAge = core.IdleSealAge

	// versionWindow is how far each open raises the version limit past the
	// floor: more versions than any one test draws.
	versionWindow = 1 << 32
)

// fixtureStart is when the fixture's clock starts.
var fixtureStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fixtureInstall is the installation ID the fixture's journals are created
// under.
var fixtureInstall = journalv2.InstallationID{'d', 'i', 't', 't', 'o', 'f', 's'}

// Fixture is one journal on a temporary directory of the real filesystem,
// reached through a crash-simulating seam, on a test clock, under the
// profiler. Every failure of its own is reported with the prefix "fixture:",
// so a test failing in it is never mistaken for one failing its own check.
//
// A Fixture is used from the test's goroutine only.
type Fixture struct {
	// J is the journal open now. Crash and Reopen replace it.
	J *journalv2.Journal

	// Seam is the storage the journal reaches its directory through: for
	// faults, holds, call counts and durable images.
	Seam *Seam

	// Dir is the journal's directory.
	Dir string

	tb      testing.TB
	clock   *sim.TestClock
	id      journalv2.JournalID
	floor   journalv2.Version // the floor the journal open now was opened with
	limit   journalv2.Version // the version limit set at the last open
	opts    []journalv2.Option
	handles map[int]*journalv2.Handle
}

// New initialises a journal in a new temporary directory and opens it, with
// two append streams, a FixtureMaxBytes maximum and segments of
// FixtureSegmentCap bytes. The journal and every handle are closed, and the
// profiler's checks run, when the test ends.
func New(tb testing.TB) *Fixture {
	tb.Helper()
	return newFixture(tb, nil, true, FixtureSegmentCap, nil)
}

// NewWith is New with set applied to the journal's settings before Init, for
// a check that needs a bound the fixture's defaults put out of reach.
func NewWith(tb testing.TB, set func(*journalv2.Settings)) *Fixture {
	tb.Helper()
	return newFixture(tb, nil, true, FixtureSegmentCap, set)
}

// NewUnwatched is New without the profiler, for a second journal in a test
// that already has one: two watches active at once both fail. Called after
// New, as it must be, this journal is closed before the first fixture's
// watch runs its checks, which then cover it too.
func NewUnwatched(tb testing.TB) *Fixture {
	tb.Helper()
	return newFixture(tb, nil, false, FixtureSegmentCap, nil)
}

// newFixture is New with fault set on the seam before the journal is
// initialised, the profiler only when watch is set, segments of segCap
// bytes, and set, when not nil, applied to the settings.
func newFixture(tb testing.TB, fault sim.Fault, watch bool, segCap int64, set func(*journalv2.Settings)) *Fixture {
	tb.Helper()

	// Registered first, so its checks run after every other cleanup has
	// closed what it opened.
	if watch {
		prof.Watch(tb, prof.Options{})
	}

	dir := tb.TempDir()
	seam, err := NewSeam(dir)
	if err != nil {
		tb.Fatalf("fixture: seam: %v", err)
	}
	seam.SetFault(fault)

	settings := journalv2.Settings{
		MaxBytes:        FixtureMaxBytes,
		Streams:         fixtureStreams,
		HeadroomRecords: fixtureHeadroomRecords,
		IdleSealBytes:   segCap / 16,
	}
	core.SetSegmentCap(&settings, segCap)
	if set != nil {
		set(&settings)
	}

	clock := sim.NewTestClock(fixtureStart)
	f := &Fixture{
		Seam:    seam,
		Dir:     dir,
		tb:      tb,
		clock:   clock,
		opts:    []journalv2.Option{journalv2.WithClock(clock), journalv2.WithSettings(settings)},
		handles: map[int]*journalv2.Handle{},
	}

	f.id, err = journalv2.Init(tb.Context(), dir, fixtureInstall, f.options()...)
	if err != nil {
		tb.Fatalf("fixture: init: %v", err)
	}

	f.open(true)
	tb.Cleanup(f.close)
	return f
}

// options returns the journal's options with a view of the seam the next
// journal opened is the only one to use.
func (f *Fixture) options() []journalv2.Option {
	return append(f.opts[:len(f.opts):len(f.opts)], journalv2.WithStorage(f.Seam.FS()))
}

// open opens the fixture's journal, raises its version limit when limit is
// set, and attaches every share attached before.
//
// The floor of each open is the limit the last one set, as a caller that
// records its limit durably before raising it would pass: no version issued
// before a crash, whose record the crash lost, is then issued again.
func (f *Fixture) open(limit bool) {
	f.tb.Helper()

	f.floor = f.limit
	j, err := journalv2.Open(f.tb.Context(), f.Dir, f.floor, f.id, f.options()...)
	if err != nil {
		f.tb.Fatalf("fixture: open: %v", err)
	}
	f.J = j

	// The journal issues no version until its first limit.
	if limit {
		f.limit = journalv2.Version{Counter: f.floor.Counter + versionWindow}
		if err := f.J.SetVersionLimit(f.limit); errors.Is(err, journalv2.ErrNotImplemented) {
			f.tb.Fatalf("stub: SetVersionLimit")
		} else if err != nil {
			f.tb.Fatalf("fixture: set version limit: %v", err)
		}
	}

	for i := range f.handles {
		f.handles[i] = f.attach(i)
	}
}

// close closes every handle, then the journal, when the test ends. A handle
// or journal already closed, as one left by an open that failed, returns
// ErrClosed, which is not reported.
func (f *Fixture) close() {
	if f.J == nil {
		return
	}

	var errs []error
	for _, h := range f.handles {
		errs = append(errs, h.Close())
	}
	errs = append(errs, f.J.Close())
	f.J = nil

	for _, err := range errs {
		if err != nil && !errors.Is(err, journalv2.ErrClosed) {
			f.tb.Errorf("fixture: close: %v", err)
		}
	}
}

// Share returns the handle of share i, attaching it on first use with tag
// i+1, no limit below the journal's, the floor the journal was opened with,
// and the Bounded policy. Crash and Reopen attach it again, so a test asks
// for it again after either.
func (f *Fixture) Share(i int) *journalv2.Handle {
	f.tb.Helper()

	if h, ok := f.handles[i]; ok {
		return h
	}
	h := f.attach(i)
	f.handles[i] = h
	return h
}

func (f *Fixture) attach(i int) *journalv2.Handle {
	f.tb.Helper()

	h, err := f.J.Share(Tag(i), 0, f.floor, journalv2.Bounded)
	if err != nil {
		f.tb.Fatalf("fixture: attach share %d: %v", i, err)
	}
	return h
}

// Floor returns the floor the journal open now was opened with: every
// version it issues exceeds it.
func (f *Fixture) Floor() journalv2.Version { return f.floor }

// Tag returns the tag of the fixture's share i.
func Tag(i int) journalv2.ShareTag {
	return journalv2.ShareTag(i + 1)
}

// File returns the ID of the fixture's file i: the same for the same i in
// every fixture.
func File(i int) journalv2.FileID {
	var id [16]byte
	copy(id[:], fmt.Sprintf("file-%011d", i))
	return journalv2.NewFileID(id)
}

// Crash simulates a power loss under the journal and opens it again on what
// the loss left. The journal from before the crash is closed, but through a
// view of the seam the crash ended, so nothing it does while closing reaches
// the directory. opts are the seam's crash variants.
//
// A crash does not release a hold, and closing the old journal waits for its
// calls in flight, so a test releases every call it holds before it crashes.
func (f *Fixture) Crash(opts ...CrashOption) {
	f.tb.Helper()

	if err := f.Seam.Crash(opts...); err != nil {
		f.tb.Fatalf("fixture: crash: %v", err)
	}

	for _, h := range f.handles {
		_ = h.Close() // a crashed process's handles return nothing anyone reads
	}
	if err := f.J.Close(); err != nil && !errors.Is(err, fs.ErrClosed) && !errors.Is(err, journalv2.ErrClosed) {
		f.tb.Logf("fixture: closing the crashed journal: %v", err)
	}

	f.open(true)
}

// Reopen closes the journal cleanly and opens it again.
func (f *Fixture) Reopen() {
	f.tb.Helper()
	f.reopen(true)
}

// ReopenWithoutLimit is Reopen without setting a version limit, so the
// journal it opens has none until the test sets one. Its floor is the same as
// Reopen's.
func (f *Fixture) ReopenWithoutLimit() {
	f.tb.Helper()
	f.reopen(false)
}

func (f *Fixture) reopen(limit bool) {
	f.tb.Helper()

	for _, h := range f.handles {
		if err := h.Close(); err != nil {
			f.tb.Fatalf("fixture: close a handle: %v", err)
		}
	}
	if err := f.J.Close(); err != nil {
		f.tb.Fatalf("fixture: close: %v", err)
	}

	f.open(limit)
}

// FailNext makes the next n calls of op on a file matching key fail with
// EIO, replacing any fault set before; every other call passes. key is a
// pattern as sim.Match takes it.
func (f *Fixture) FailNext(op, key string, n int) {
	f.tb.Helper()

	fault, err := sim.NewSchedule(1, f.clock, sim.Rule{
		Op: op, Key: key, Count: n,
		Action: sim.FaultAction{Kind: sim.Fail, Err: syscall.EIO},
	})
	if err != nil {
		f.tb.Fatalf("fixture: fail %s on %s: %v", op, key, err)
	}
	f.Seam.SetFault(fault)
}

// Drop makes every call of op on a file matching key report success and make
// nothing durable, replacing any fault set before. op is one the seam can
// drop: write, sync or sync_dir.
func (f *Fixture) Drop(op, key string) {
	f.tb.Helper()

	fault, err := sim.NewSchedule(1, f.clock, sim.Rule{Op: op, Key: key, Action: sim.FaultAction{Kind: sim.Drop}})
	if err != nil {
		f.tb.Fatalf("fixture: drop %s on %s: %v", op, key, err)
	}
	f.Seam.SetFault(fault)
}

// Advance moves the journal's clock by d, firing every timer it reaches.
func (f *Fixture) Advance(d time.Duration) { f.clock.Advance(d) }

// Jump steps the journal's wall clock by d, firing no timer.
func (f *Fixture) Jump(d time.Duration) { f.clock.Jump(d) }

// Now returns the journal's wall time.
func (f *Fixture) Now() time.Time { return f.clock.Now() }

// Allocated returns the storage the filesystem has allocated to the files of
// the journal's directory, in bytes: every file but the format file, whose
// few bytes a journal's accounting does not cover.
//
// The seam's Allocate sets a file's size without reserving its blocks, so
// what this reads is the blocks the journal's writes filled, zero-filling
// included: a check against it catches accounting below what is written,
// never a filesystem's speculative allocation past the end of a growing file.
func (f *Fixture) Allocated() int64 {
	f.tb.Helper()

	n, err := allocated(f.Dir)
	if err != nil {
		f.tb.Fatalf("fixture: allocated storage: %v", err)
	}
	return n
}
