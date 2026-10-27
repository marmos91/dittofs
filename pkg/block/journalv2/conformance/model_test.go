//go:build journal_conformance

package conformance

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/model"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// modelSteps is how many steps the seeded sequence of TestModel runs.
//
// ponytail: one sequence of 200 steps per run, comparing every file after
// every step, which keeps the run within a few seconds. Many seeds and longer
// sequences belong in the tiers that run for longer; raise this only if a
// failure needs a longer sequence to show and no corpus file holds it.
const modelSteps = 200

// modelSeed is the seed of TestModel's sequence.
//
// decision: the seed is DITTOFS_SEED when it is set, and a fixed one
// otherwise. A run that is retried inherits the same environment, so it draws
// the same sequence again and cannot pass by drawing another: a failing seed
// stays failing until the journal is fixed and its shrunk sequence committed.
// Only a tier that sets DITTOFS_SEED from a fresh draw explores new sequences.
func modelSeed(t *testing.T) uint64 {
	t.Helper()

	s, ok := os.LookupEnv("DITTOFS_SEED")
	if !ok {
		return 1
	}
	seed, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("DITTOFS_SEED=%q: %v", s, err)
	}
	return seed
}

// The journal agrees with its reference model after every step of random
// sequences of writes, syncs, reads, fills, offloads, releases, truncates,
// deallocates, deletes, settles, repacks, crashes of every kind the seam
// simulates, and clean reopens, over four files of two shares on one journal.
// Every sequence kept in testdata/model runs first, then the seeded one. A
// failing sequence is shrunk and written to the test's artifact directory, to
// be committed to testdata/model with the fix.
func TestModel(t *testing.T) {
	prof.Watch(t, prof.Options{})

	corpus, err := model.ReadCorpus("testdata/model")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	for _, seq := range corpus {
		model.Check(t, seq.Name, seq.Steps, newModelTarget(t), "kept as "+seq.Name)
	}

	seed := modelSeed(t)
	model.Check(t, fmt.Sprintf("seed-%d.txt", seed), model.Generate(seed, modelSteps), newModelTarget(t),
		fmt.Sprintf("drawn from seed %d, %d steps", seed, modelSteps))
}

// newModelTarget returns the function that opens a fresh fixture journal for
// each sequence the model-based test runs, shrinking attempts included.
func newModelTarget(t *testing.T) func() (model.Target, func(), error) {
	return func() (model.Target, func(), error) {
		tb := &attemptTB{TB: t}
		var tgt *fixtureTarget
		err := tb.guard(func() {
			f := journalv2test.NewUnwatched(tb)
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
func (a *attemptTB) FailNow()         { panic(attemptFailed{"FailNow"}) }
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
	f  *journalv2test.Fixture
	tb *attemptTB
}

func (x *fixtureTarget) Share(i int) model.Handle { return spareRetry{x.f.Share(i)} }

func (x *fixtureTarget) View(i int) model.View {
	return model.HandleView{Handle: x.f.Share(i), Journal: x.f.J}
}

func (x *fixtureTarget) Repack(ctx context.Context) error {
	_, err := x.f.J.Repack(ctx, 2*journalv2test.FixtureSegmentCap, journalv2.RepackScope{})
	return err
}

func (x *fixtureTarget) Crash(kind model.CrashKind, arg uint64) error {
	var opts []journalv2test.CrashOption
	switch kind {
	case model.CrashTorn:
		opts = append(opts, journalv2test.Torn(int(arg)))
	case model.CrashReordered:
		opts = append(opts, journalv2test.Reordered(arg))
	}
	return x.tb.guard(func() { x.f.Crash(opts...) })
}

func (x *fixtureTarget) Reopen() error { return x.tb.guard(x.f.Reopen) }

func (x *fixtureTarget) Floor() journalv2.Version { return x.f.Floor() }

// spareRetry retries every appending call refused for want of a spare
// segment, as its caller would, so that refusal never reaches the model.
type spareRetry struct{ *journalv2.Handle }

func (h spareRetry) WriteAt(ctx context.Context, id journalv2.FileID, off int64, p []byte, mtime int64) (v journalv2.Version, err error) {
	err = retrySpare(func() error { v, err = h.Handle.WriteAt(ctx, id, off, p, mtime); return err })
	return v, err
}

func (h spareRetry) Fill(ctx context.Context, id journalv2.FileID, off int64, p []byte, asOf, v journalv2.Version) error {
	return retrySpare(func() error { return h.Handle.Fill(ctx, id, off, p, asOf, v) })
}

func (h spareRetry) Release(ctx context.Context, id journalv2.FileID, extents []journalv2.Extent) error {
	return retrySpare(func() error { return h.Handle.Release(ctx, id, extents) })
}

func (h spareRetry) Truncate(ctx context.Context, id journalv2.FileID, size int64) (v journalv2.Version, err error) {
	err = retrySpare(func() error { v, err = h.Handle.Truncate(ctx, id, size); return err })
	return v, err
}

func (h spareRetry) Deallocate(ctx context.Context, id journalv2.FileID, e journalv2.Extent) (v journalv2.Version, err error) {
	err = retrySpare(func() error { v, err = h.Handle.Deallocate(ctx, id, e); return err })
	return v, err
}

func (h spareRetry) Delete(ctx context.Context, id journalv2.FileID) (v journalv2.Version, err error) {
	err = retrySpare(func() error { v, err = h.Handle.Delete(ctx, id); return err })
	return v, err
}
