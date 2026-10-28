package journalv2test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/model"
)

// ModelTargets returns open, the function that opens a fresh fixture
// journal, with shares 0 and 1 attached, for each sequence the model-based
// test runs, shrinking attempts included. A failure of the fixture's ends
// that sequence with an error rather than the test, and what the sequence
// opened is closed and removed when it ends. The fixtures are unwatched: the
// test watches itself once, before calling it.
//
// It also returns streams, the most append streams the records of any one
// sequence were on when it ended, which the journal chooses and the test can
// only observe.
func ModelTargets(t testing.TB) (open func() (model.Target, func(), error), streams func() int) {
	parent := t.TempDir()
	most := 0
	streams = func() int { return most }
	open = func() (model.Target, func(), error) {
		tb := &attemptTB{TB: t, parent: parent}
		var tgt *fixtureTarget
		err := tb.guard(func() {
			f := NewUnwatched(tb)
			f.Share(0)
			f.Share(1)
			tgt = &fixtureTarget{f: f, tb: tb}
		})
		if err != nil {
			tb.finish()
			return nil, nil, err
		}
		done := func() {
			most = max(most, tgt.streams())
			tb.finish()
		}
		return tgt, done, nil
	}
	return open, streams
}

// attemptFailed is the panic an attemptTB raises for a failure, so that one
// sequence's fixture failing ends that sequence rather than the test. It is
// also the error guard returns: its message, wrapping every error the failure
// was reported with, so errors.Is sees the journal's error through it.
type attemptFailed struct {
	msg  string
	errs []error
}

func (e attemptFailed) Error() string   { return e.msg }
func (e attemptFailed) Unwrap() []error { return e.errs }

func failure(msg string, args []any) attemptFailed {
	f := attemptFailed{msg: msg}
	for _, a := range args {
		if err, ok := a.(error); ok {
			f.errs = append(f.errs, err)
		}
	}
	return f
}

// attemptTB is the testing.TB of one sequence's fixture. A failure the
// fixture reports panics with attemptFailed, which guard turns into an error;
// cleanups run when the sequence ends, not when the test does.
type attemptTB struct {
	testing.TB
	parent   string // where the sequence's directories are made
	cleanups []func()
}

func (a *attemptTB) Helper()          {}
func (a *attemptTB) Cleanup(f func()) { a.cleanups = append(a.cleanups, f) }

// TempDir returns a new directory that is removed when the sequence ends, so
// a test running many sequences holds the storage of one at a time.
func (a *attemptTB) TempDir() string {
	dir, err := os.MkdirTemp(a.parent, "sequence")
	if err != nil {
		a.Fatalf("fixture: temporary directory: %v", err)
	}
	a.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func (a *attemptTB) FailNow()          { panic(attemptFailed{msg: "FailNow"}) }
func (a *attemptTB) Fatal(args ...any) { panic(failure(fmt.Sprint(args...), args)) }
func (a *attemptTB) Error(args ...any) { panic(failure(fmt.Sprint(args...), args)) }
func (a *attemptTB) Fatalf(format string, args ...any) {
	panic(failure(fmt.Sprintf(format, args...), args))
}
func (a *attemptTB) Errorf(format string, args ...any) {
	panic(failure(fmt.Sprintf(format, args...), args))
}

// guard runs fn and returns the failure it reported, if any.
func (a *attemptTB) guard(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			failed, ok := r.(attemptFailed)
			if !ok {
				panic(r)
			}
			err = failed
		}
	}()
	fn()
	return nil
}

// finish runs the sequence's cleanups, last first, as the testing package
// does. A failure of one is reported to the test, since what a closing
// journal reports is a check of its own.
func (a *attemptTB) finish() {
	for _, f := range slices.Backward(a.cleanups) {
		if err := a.guard(f); err != nil {
			a.TB.Errorf("%v", err)
		}
	}
	a.cleanups = nil
}

// fixtureTarget runs model-based sequences against a fixture journal.
type fixtureTarget struct {
	f  *Fixture
	tb *attemptTB
}

func (x *fixtureTarget) Share(i int) model.Handle { return spareRetry{x.f.Share(i)} }

func (x *fixtureTarget) View(i int) model.View {
	return model.HandleView{Handle: x.f.Share(i), Journal: x.f.J}
}

func (x *fixtureTarget) Repack(ctx context.Context) error {
	_, err := x.f.J.Repack(ctx, 2*FixtureSegmentCap, journalv2.RepackScope{})
	return err
}

func (x *fixtureTarget) Crash(kind model.CrashKind, arg uint64) error {
	var opts []CrashOption
	switch kind {
	case model.CrashTorn:
		opts = append(opts, Torn(int(arg)))
	case model.CrashReordered:
		opts = append(opts, Reordered(arg))
	}
	return reopened(x.tb.guard(func() { x.f.Crash(opts...) }))
}

func (x *fixtureTarget) Reopen() error { return reopened(x.tb.guard(x.f.Reopen)) }

// journalFailures are the prefixes of the fixture's failures, during a crash
// or a reopen, that are the journal's: closing it or a handle, opening it
// again, and attaching its shares again.
var journalFailures = []string{"fixture: close", "fixture: open:", "fixture: attach share"}

// reopened returns err, the failure of a crash or a reopen, as a
// model.ReopenError when the journal failed to close or to open again.
//
// decision: the journal's failures are told apart by the prefix of the
// message the fixture reports them with, since it reports every failure
// through the test. Anything else, such as the seam failing to crash, stays
// the target's own error. Give the fixture a crash and a reopen that return
// their errors if a second caller needs to tell them apart.
func reopened(err error) error {
	if err != nil && slices.ContainsFunc(journalFailures, func(p string) bool { return strings.HasPrefix(err.Error(), p) }) {
		return model.ReopenError{Err: err}
	}
	return err
}

func (x *fixtureTarget) Floor() journalv2.Version { return x.f.Floor() }

// streams returns how many append streams hold the records the journal's
// index points at now: none when no journal is open, as after a reopen that
// failed.
func (x *fixtureTarget) streams() int {
	if x.f.J == nil {
		return 0
	}
	in := x.f.J.Inspect()
	of := map[journalv2.SegmentID]uint32{}
	for _, s := range in.Segments() {
		of[s.ID] = s.Stream
	}
	used := map[uint32]bool{}
	for _, entries := range in.All() {
		for _, e := range entries {
			if e.Kind != journalv2.EntryMarker {
				if stream, ok := of[e.Location.Segment]; ok {
					used[stream] = true
				}
			}
		}
	}
	return len(used)
}

// spareRetry retries every appending call refused for want of a spare
// segment, as its caller would, so that refusal never reaches the model.
type spareRetry struct{ *journalv2.Handle }

func (h spareRetry) WriteAt(ctx context.Context, id journalv2.FileID, off int64, p []byte, mtime int64) (v journalv2.Version, err error) {
	err = RetrySpare(func() error { v, err = h.Handle.WriteAt(ctx, id, off, p, mtime); return err })
	return v, err
}

func (h spareRetry) Fill(ctx context.Context, id journalv2.FileID, off int64, p []byte, asOf, v journalv2.Version) error {
	return RetrySpare(func() error { return h.Handle.Fill(ctx, id, off, p, asOf, v) })
}

func (h spareRetry) Release(ctx context.Context, id journalv2.FileID, extents []journalv2.Extent) error {
	return RetrySpare(func() error { return h.Handle.Release(ctx, id, extents) })
}

func (h spareRetry) Truncate(ctx context.Context, id journalv2.FileID, size int64) (v journalv2.Version, err error) {
	err = RetrySpare(func() error { v, err = h.Handle.Truncate(ctx, id, size); return err })
	return v, err
}

func (h spareRetry) Deallocate(ctx context.Context, id journalv2.FileID, e journalv2.Extent) (v journalv2.Version, err error) {
	err = RetrySpare(func() error { v, err = h.Handle.Deallocate(ctx, id, e); return err })
	return v, err
}

func (h spareRetry) Delete(ctx context.Context, id journalv2.FileID) (v journalv2.Version, err error) {
	err = RetrySpare(func() error { v, err = h.Handle.Delete(ctx, id); return err })
	return v, err
}
