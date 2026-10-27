package journalv2test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/model"
)

// ModelTargets returns the function that opens a fresh fixture journal, with
// shares 0 and 1 attached, for each sequence the model-based test runs,
// shrinking attempts included. A failure of the fixture's ends that sequence
// with an error rather than the test, and what the sequence opened is closed
// and removed when it ends. The fixtures are unwatched: the test watches
// itself once, before calling it.
func ModelTargets(t testing.TB) func() (model.Target, func(), error) {
	return func() (model.Target, func(), error) {
		tb := &attemptTB{TB: t}
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
		return tgt, tb.finish, nil
	}
}

// attemptFailed is the panic an attemptTB raises for a failure, so that one
// sequence's fixture failing ends that sequence rather than the test.
type attemptFailed struct{ msg string }

// attemptTB is the testing.TB of one sequence's fixture. A failure the
// fixture reports panics with attemptFailed, which guard turns into an error;
// cleanups run when the sequence ends, not when the test does.
type attemptTB struct {
	testing.TB
	cleanups []func()
}

func (a *attemptTB) Helper()          {}
func (a *attemptTB) Cleanup(f func()) { a.cleanups = append(a.cleanups, f) }

// TempDir returns a new directory that is removed when the sequence ends, so
// a test running many sequences holds the storage of one at a time.
func (a *attemptTB) TempDir() string {
	dir, err := os.MkdirTemp(a.TB.TempDir(), "sequence")
	if err != nil {
		a.Fatalf("fixture: temporary directory: %v", err)
	}
	a.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
func (a *attemptTB) FailNow() { panic(attemptFailed{"FailNow"}) }
func (a *attemptTB) Fatal(args ...any) {
	panic(attemptFailed{fmt.Sprint(args...)})
}
func (a *attemptTB) Fatalf(format string, args ...any) {
	panic(attemptFailed{fmt.Sprintf(format, args...)})
}
func (a *attemptTB) Error(args ...any) {
	panic(attemptFailed{fmt.Sprint(args...)})
}
func (a *attemptTB) Errorf(format string, args ...any) {
	panic(attemptFailed{fmt.Sprintf(format, args...)})
}

// guard runs fn and returns the failure it reported, if any.
func (a *attemptTB) guard(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			failed, ok := r.(attemptFailed)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("%s", failed.msg)
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
	return x.tb.guard(func() { x.f.Crash(opts...) })
}

func (x *fixtureTarget) Reopen() error { return x.tb.guard(x.f.Reopen) }

func (x *fixtureTarget) Floor() journalv2.Version { return x.f.Floor() }

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
