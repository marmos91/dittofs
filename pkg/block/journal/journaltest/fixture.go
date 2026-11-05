package journaltest

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
	"github.com/cubbit/dittofs/pkg/sim"
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
	// seals it, if it is more than FixtureIdleSealBytes full.
	IdleSealAge = core.IdleSealAge

	// OfferPins is the most segments one offer pins. JournalPins is the count
	// of segments all running offers pin together past which an offer takes
	// no extent from an unpinned segment but its first.
	OfferPins   = core.OfferPins
	JournalPins = core.JournalPins

	// versionWindow is how far each open raises the version limit past the
	// floor: more versions than any one test draws.
	versionWindow = 1 << 32
)

// fixtureStart is when the fixture's clock starts.
var fixtureStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// FixtureInstall is the installation ID the fixture's journals are created
// under.
var FixtureInstall = journal.InstallationID{'d', 'i', 't', 't', 'o', 'f', 's'}

// Fixture is one journal on a temporary directory of the real filesystem,
// reached through a crash-simulating seam, on a test clock, under the
// profiler. Every failure of its own is reported with the prefix "fixture:",
// so a test failing in it is never mistaken for one failing its own check.
//
// A Fixture is used from the test's goroutine only.
type Fixture struct {
	// J is the journal open now. Crash and Reopen replace it.
	J *journal.Journal

	// Seam is the storage the journal reaches its directory through: for
	// faults, holds, call counts and durable images.
	Seam *Seam

	// Dir is the journal's directory.
	Dir string

	tb      testing.TB
	clock   *sim.TestClock
	id      journal.JournalID
	floor   journal.Version // the floor the journal open now was opened with
	limit   journal.Version // the version limit set at the last open
	opts    []journal.Option
	handles map[int]*journal.Handle
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
func NewWith(tb testing.TB, set func(*journal.Settings)) *Fixture {
	tb.Helper()
	return newFixture(tb, nil, true, FixtureSegmentCap, set)
}

// NewWithSegmentCap is New with segments of segmentCap bytes, for a check
// whose record must fit whole in one segment beside others: a write larger
// than what its segment has left is split into records. The journal's
// maximum starts at FixtureMaxBytes and whatever set changes it to: set is
// applied to the settings after the cap, so a check can settle the bounds
// the cap alone does not reach; nil sets no setting.
func NewWithSegmentCap(tb testing.TB, segmentCap int64, set func(*journal.Settings)) *Fixture {
	tb.Helper()
	return newFixture(tb, nil, true, segmentCap, set)
}

// NewWithOptions is NewWith with opts added to the options the journal is
// initialised and opened with, for a check that needs one the fixture does
// not set, such as a descriptor budget of its own. The fixture's clock, its
// settings and its storage are set after opts, so opts cannot replace them.
func NewWithOptions(tb testing.TB, set func(*journal.Settings), opts ...journal.Option) *Fixture {
	tb.Helper()
	return newFixture(tb, nil, true, FixtureSegmentCap, set, opts...)
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
// bytes, set, when not nil, applied to the settings, and opts added before
// the fixture's own options.
func newFixture(tb testing.TB, fault sim.Fault, watch bool, segCap int64, set func(*journal.Settings), opts ...journal.Option) *Fixture {
	tb.Helper()

	// Registered first, so its checks run after every other cleanup has
	// closed what it opened.
	if watch {
		prof.Watch(tb, prof.Options{})
	}

	f := bareFixture(tb, fault, segCap, set, opts...)
	var err error
	f.id, err = journal.Init(tb.Context(), f.Dir, FixtureInstall, f.options()...)
	if err != nil {
		tb.Fatalf("fixture: init: %v", err)
	}

	f.open(true)
	return f
}

// bareFixture is a fixture on a new, empty directory with no journal in it:
// the seam, the clock and the options are set and nothing is initialised or
// opened, with segments of segCap bytes, set, when not nil, applied to the
// settings, and opts added before the fixture's own options. What it opens
// later is closed when the test ends.
func bareFixture(tb testing.TB, fault sim.Fault, segCap int64, set func(*journal.Settings), opts ...journal.Option) *Fixture {
	tb.Helper()

	dir := tb.TempDir()
	seam, err := NewSeam(dir)
	if err != nil {
		tb.Fatalf("fixture: seam: %v", err)
	}
	seam.SetFault(fault)

	settings := journal.Settings{
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
		opts:    append(opts[:len(opts):len(opts)], journal.WithClock(clock), journal.WithSettings(settings)),
		handles: map[int]*journal.Handle{},
	}
	tb.Cleanup(f.close)
	return f
}

// options returns the journal's options with a view of the seam the next
// journal opened is the only one to use.
func (f *Fixture) options() []journal.Option {
	return append(f.opts[:len(f.opts):len(f.opts)], journal.WithStorage(f.Seam.FS()))
}

// open opens the fixture's journal, raises its version limit when limit is
// set, and attaches every share attached before.
func (f *Fixture) open(limit bool) {
	f.tb.Helper()

	if err := f.tryOpen(f.id, limit); err != nil {
		f.tb.Fatalf("fixture: open: %v", err)
	}
}

// tryOpen is open with expect as the identity the open expects, returning
// Open's error instead of failing: on an error nothing is open and J is nil.
//
// The floor of each open is the limit the last one set, as a caller that
// records its limit durably before raising it would pass: no version issued
// before a crash, whose record the crash lost, is then issued again.
func (f *Fixture) tryOpen(expect journal.JournalID, limit bool) error {
	f.tb.Helper()

	f.floor = f.limit
	j, err := journal.Open(f.tb.Context(), f.Dir, f.floor, expect, f.options()...)
	if err != nil {
		return err
	}
	f.J = j

	// The journal issues no version until its first limit. A limit or a
	// share refused shuts the journal again, so an error leaves nothing
	// open; the caller, on the test's goroutine, is what fails the test.
	if limit {
		f.limit = journal.Version{Counter: f.floor.Counter + versionWindow}
		if err := f.J.SetVersionLimit(f.limit); err != nil {
			f.dropOpen()
			return fmt.Errorf("set version limit %v: %w", f.limit, err)
		}
	}

	for i := range f.handles {
		h, err := f.attach(i)
		if err != nil {
			f.dropOpen()
			return fmt.Errorf("attach share %d: %w", i, err)
		}
		f.handles[i] = h
	}

	return nil
}

// dropOpen closes a journal an open could not finish setting up, and
// forgets it, so a failed open leaves J nil as its contract says.
func (f *Fixture) dropOpen() {
	if err := f.J.Close(); err != nil && !errors.Is(err, journal.ErrClosed) {
		f.tb.Logf("fixture: closing a journal whose open failed: %v", err)
	}

	f.J = nil
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
		if err != nil && !errors.Is(err, journal.ErrClosed) {
			f.tb.Errorf("fixture: close: %v", err)
		}
	}
}

// Abandon leaves the journal open now, and its handles, unclosed when the test
// ends, for a test that found a call blocked in it: closing would wait for
// that call, and the test would hang rather than fail. The journal's
// goroutines and descriptors are then leaked, and the profiler reports them.
func (f *Fixture) Abandon() {
	f.J = nil
	clear(f.handles)
}

// Share returns the handle of share i, attaching it on first use with tag
// i+1, no limit below the journal's, the floor the journal was opened with,
// and the Bounded policy. Crash and Reopen attach it again, so a test asks
// for it again after either. It is not called while the journal is shut.
func (f *Fixture) Share(i int) *journal.Handle {
	f.tb.Helper()

	if h, ok := f.handles[i]; ok {
		return h
	}

	h, err := f.attach(i)
	if err != nil {
		f.tb.Fatalf("fixture: attach share %d: %v", i, err)
	}
	f.handles[i] = h
	return h
}

// attach attaches share i to the open journal and returns its handle, or
// the journal's refusal; it fails nothing itself, since an open may run it
// off the test's goroutine.
func (f *Fixture) attach(i int) (*journal.Handle, error) {
	return f.J.Share(Tag(i), 0, f.floor, journal.Bounded)
}

// Floor returns the floor the journal open now was opened with: every
// version it issues exceeds it.
func (f *Fixture) Floor() journal.Version { return f.floor }

// ID returns the identity the fixture's journal was initialised with, the one
// every open of it expects.
func (f *Fixture) ID() journal.JournalID { return f.id }

// Tag returns the tag of the fixture's share i.
func Tag(i int) journal.ShareTag {
	return journal.ShareTag(i + 1)
}

// File returns the ID of the fixture's file i: the same for the same i in
// every fixture.
func File(i int) journal.FileID {
	var id [16]byte
	copy(id[:], fmt.Sprintf("file-%011d", i))
	return journal.NewFileID(id)
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

	if err := f.TryCrash(opts...); err != nil {
		f.tb.Fatalf("fixture: crash: %v", err)
	}
}

// Kill ends the process holding the journal without a storage loss, as a
// kill does: the seam stops serving the journal's view and closes its files,
// every byte written stays readable as the operating system's cache keeps a
// dead process's writes, and what no sync covered is still lost by a later
// Crash. Then the journal is opened again on what the process left, through
// the path Reopen and Crash take.
func (f *Fixture) Kill() {
	f.tb.Helper()

	if err := f.TryKill(); err != nil {
		f.tb.Fatalf("fixture: kill: %v", err)
	}
}

// Reopen closes the journal cleanly and opens it again.
func (f *Fixture) Reopen() {
	f.tb.Helper()
	f.reopen(true)
}

// ReopenWithoutLimit is Reopen without setting a version limit, so the
// journal it opens has none until the test sets one. Its floor is the same as
// Reopen's. A limit the test sets through f.J is not recorded: a later Reopen
// or Crash takes its floor from the last limit the fixture set, not that one.
func (f *Fixture) ReopenWithoutLimit() {
	f.tb.Helper()
	f.reopen(false)
}

// reopen closes every handle and the journal, failing the test if one does
// not close cleanly, and opens the journal again, setting a version limit
// when limit is set.
func (f *Fixture) reopen(limit bool) {
	f.tb.Helper()

	f.Shutdown()
	f.open(limit)
}

// Shutdown closes every handle and the journal cleanly and leaves nothing
// open, so a test can edit the directory with the journal shut; Reopen,
// TryReopen and Crash open it again. A journal already shut is left as it is.
func (f *Fixture) Shutdown() {
	f.tb.Helper()

	if f.J == nil {
		return
	}
	for _, h := range f.handles {
		if err := h.Close(); err != nil {
			f.tb.Fatalf("fixture: close a handle: %v", err)
		}
	}
	if err := f.J.Close(); err != nil {
		f.tb.Fatalf("fixture: close: %v", err)
	}
	f.J = nil
}

// TryCrash is Crash returning the seam's refusal, or Open's error, instead
// of failing the test: after an error the journal is shut, J is nil, until a
// later open succeeds. It is for a check that holds a call of the open after
// a crash and crashes again there, so the open fails as a dead process's
// would.
func (f *Fixture) TryCrash(opts ...CrashOption) error {
	f.tb.Helper()

	if err := f.Seam.Crash(opts...); err != nil {
		return err
	}
	f.dropDead()
	return f.tryOpen(f.id, true)
}

// TryKill is Kill returning Open's error instead of failing the test: after
// an error the journal is shut, J is nil, until a later open succeeds.
//
// decision: the killed journal is not closed. A dead process closes, seals
// and syncs nothing, and a close through the dead view would still count its
// calls on the files the seam closed. So a goroutine of the old journal
// parked on the fixture's clock or on a channel makes no seam call, never
// fails on the dead view and can outlive it, and the profiler would report
// it as a leak on a correct journal: the checks that kill run on NewUnwatched
// fixtures. Close the old journal outside the seam's counts, and watch them
// again, once the seam can tell such a close from the journal's own calls.
func (f *Fixture) TryKill() error {
	f.tb.Helper()

	f.Seam.Abandon()
	f.J = nil // not closed: a dead process closes nothing
	return f.tryOpen(f.id, true)
}

// dropDead closes the handles and the journal the seam's crash left dead,
// through its dead view, so nothing reaches the directory, and forgets it.
func (f *Fixture) dropDead() {
	if f.J == nil {
		return
	}

	for _, h := range f.handles {
		_ = h.Close() // a crashed process's handles return nothing anyone reads
	}

	if err := f.J.Close(); err != nil && !errors.Is(err, fs.ErrClosed) && !errors.Is(err, journal.ErrClosed) {
		f.tb.Logf("fixture: closing the crashed journal: %v", err)
	}

	f.J = nil
}

// TryReopen is Reopen returning Open's error instead of failing the test:
// after an error the journal is shut, J is nil, until a later open succeeds.
func (f *Fixture) TryReopen() error {
	f.tb.Helper()
	return f.TryReopenAs(f.id)
}

// TryReopenAs is TryReopen with expect as the identity the open expects, the
// zero identity included. The fixture's own identity is unchanged, so a
// later Reopen expects it again.
func (f *Fixture) TryReopenAs(expect journal.JournalID) error {
	f.tb.Helper()

	f.Shutdown()
	return f.tryOpen(expect, true)
}

// TryInit runs Init on the fixture's directory with its options and returns
// what Init returns. On success the identity becomes the fixture's, the one a
// later open expects; the journal open now, if any, is not replaced.
func (f *Fixture) TryInit() (journal.JournalID, error) {
	f.tb.Helper()

	id, err := journal.Init(f.tb.Context(), f.Dir, FixtureInstall, f.options()...)
	if err == nil {
		f.id = id
	}
	return id, err
}

// TryRestamp shuts the journal down and runs Restamp on its directory with
// the fixture's identity, install and options, and returns what Restamp
// returns. The journal stays shut; Reopen or Crash opens it again.
func (f *Fixture) TryRestamp(install journal.InstallationID) error {
	f.tb.Helper()

	f.Shutdown()
	return journal.Restamp(f.tb.Context(), f.Dir, f.id, install, f.options()...)
}

// FailNext makes the next n calls of op on a file matching key fail with
// EIO, replacing any fault set before; every other call passes. key is a
// pattern as sim.Match takes it.
func (f *Fixture) FailNext(op, key string, n int) {
	f.tb.Helper()

	fault, err := sim.NewSchedule(1, f.clock, sim.Rule{
		Op: op, Key: key, Count: n,
		Action: sim.FaultAction{Err: syscall.EIO},
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

	fault, err := sim.NewSchedule(1, f.clock, sim.Rule{Op: op, Key: key, Action: sim.FaultAction{TearAfter: sim.Lose}})
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
