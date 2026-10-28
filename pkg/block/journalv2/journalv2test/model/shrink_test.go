package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// Shrinking keeps only the steps the failure needs, however far apart they
// are in the sequence.
func TestShrinkKeepsOnlyWhatTheFailureNeeds(t *testing.T) {
	steps := Generate(3, 200)
	steps[40] = Step{Op: OpDelete, File: 2}
	steps[170] = Step{Op: OpTruncate, File: 2, Off: 9}
	// It fails while a delete of file 2 comes before a truncate of it.
	fails := func(c []Step) bool {
		i := slices.IndexFunc(c, func(s Step) bool { return s.Op == OpDelete && s.File == 2 })
		return i >= 0 && slices.ContainsFunc(c[i:], func(s Step) bool { return s.Op == OpTruncate && s.File == 2 })
	}
	got := shrink(steps, fails, time.Now().Add(time.Minute))
	want := []Step{{Op: OpDelete, File: 2}, {Op: OpTruncate, File: 2, Off: 9}}
	if !slices.Equal(got, want) {
		t.Fatalf("shrunk to\n%s\nwant\n%s", formatCorpus(got), formatCorpus(want))
	}
}

// Shrinking also shortens the extents of the steps it keeps.
func TestShrinkShortensExtents(t *testing.T) {
	fails := func(c []Step) bool {
		return slices.ContainsFunc(c, func(s Step) bool { return s.Op == OpWrite && s.Len >= 3 })
	}
	got := shrink([]Step{{Op: OpWrite, Len: 1000}}, fails, time.Now().Add(time.Minute))
	if len(got) != 1 || got[0].Len >= 6 {
		t.Fatalf("shrunk to\n%s\nwant one write shorter than 6 bytes", formatCorpus(got))
	}
}

// recorder is a testing.TB that keeps the failures Check reports, so a test
// can look at them instead of failing.
type recorder struct {
	testing.TB
	dir    string
	errors []string
}

func (r *recorder) Helper()             {}
func (r *recorder) ArtifactDir() string { return r.dir }
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// flipView serves the first held byte of every read of file 1 with its low
// bit flipped: a journal returning one wrong byte.
type flipView struct{ View }

func (v flipView) ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	n, missing, held, asOf, err := v.View.ReadAt(ctx, id, off, p)
	if id == fileOf(1) && len(held) > 0 {
		p[held[0].Extent.Off-off] ^= 1
	}
	return n, missing, held, asOf, err
}

type flipTarget struct{ Target }

func (t flipTarget) View(i int) View { return flipView{t.Target.View(i)} }

// A journal returning one wrong byte is caught, and the sequence that shows it
// is shrunk to a few steps and written as a corpus file that fails again when
// replayed, and does not against an honest journal.
func TestCheckShrinksAPlantedDivergence(t *testing.T) {
	rec := &recorder{TB: t, dir: t.TempDir()}
	Check(rec, "planted.txt", Generate(0, 200), func() (Target, func(), error) {
		return flipTarget{newPlayed(t)}, func() {}, nil
	})
	if len(rec.errors) != 1 {
		t.Fatalf("want the divergence reported once, got %q", rec.errors)
	}
	t.Log(rec.errors[0])

	text, err := os.ReadFile(filepath.Join(rec.dir, "planted.txt"))
	ok(t, err)
	shrunk, err := parseCorpus(text)
	ok(t, err)
	if len(shrunk) > 3 {
		t.Fatalf("shrunk to %d steps, want at most 3:\n%s", len(shrunk), text)
	}
	if _, err := runSteps(t.Context(), flipTarget{newPlayed(t)}, shrunk); !errors.Is(err, ErrDiverged) {
		t.Fatalf("the corpus file replayed with %v, want a divergence", err)
	}
	if _, err := runSteps(t.Context(), newPlayed(t), shrunk); err != nil {
		t.Fatalf("the corpus file fails against an honest journal: %v", err)
	}
}

// lowFloor reopens with a floor below the versions already issued, which the
// model refuses as a call it cannot take.
type lowFloor struct{ *played }

func (lowFloor) Floor() journalv2.Version { return journalv2.Version{} }

// A sequence that drives the model where it cannot follow is the driver's
// bug, not the journal's: it is reported as it is, never shrunk into a corpus
// file.
func TestCheckDoesNotShrinkAMisuse(t *testing.T) {
	rec := &recorder{TB: t, dir: t.TempDir()}
	steps := []Step{{Op: OpWrite, Len: 1}, {Op: OpSync}, {Op: OpWrite, Len: 1}, {Op: OpCrash}}
	Check(rec, "misuse.txt", steps, func() (Target, func(), error) {
		return lowFloor{newPlayed(t)}, func() {}, nil
	})
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], ErrMisuse.Error()) {
		t.Fatalf("want the misuse reported, got %q", rec.errors)
	}
	if _, err := os.Stat(filepath.Join(rec.dir, "misuse.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a misuse was written as a corpus file: %v", err)
	}
}

// A divergence that does not come back on the shrunk sequence is kept whole,
// so the corpus file written still shows a failure.
func TestCheckKeepsADivergenceShrinkingLoses(t *testing.T) {
	rec := &recorder{TB: t, dir: t.TempDir()}
	steps := Generate(0, 50)
	opened := 0
	Check(rec, "once.txt", steps, func() (Target, func(), error) {
		// The run and the first two shrinking attempts fail; later runs,
		// the rerun of the shrunk sequence among them, do not.
		opened++
		if opened <= 3 {
			return flipTarget{newPlayed(t)}, func() {}, nil
		}
		return newPlayed(t), func() {}, nil
	})
	if len(rec.errors) != 1 {
		t.Fatalf("want the divergence reported once, got %q", rec.errors)
	}
	text, err := os.ReadFile(filepath.Join(rec.dir, "once.txt"))
	ok(t, err)
	kept, err := parseCorpus(text)
	ok(t, err)
	if !slices.Equal(kept, steps) {
		t.Fatalf("wrote %d steps, want the %d that failed:\n%s", len(kept), len(steps), text)
	}
}

// stubbed answers Truncate as a journal call not written yet.
type stubbed struct{ *played }

func (s stubbed) Share(i int) Handle { return stubbedHandle{s.played.Share(i)} }

type stubbedHandle struct{ Handle }

func (stubbedHandle) Truncate(context.Context, journalv2.FileID, int64) (journalv2.Version, error) {
	return journalv2.Version{}, journalv2.ErrNotImplemented
}

// fatalRecorder is a recorder whose Fatalf records too, so a test can see
// what Check ends a test with.
type fatalRecorder struct{ recorder }

func (r *fatalRecorder) Fatalf(format string, args ...any) { r.Errorf(format, args...) }

// A journal call not written yet ends the test as "stub: <call>", never as a
// divergence to shrink into a corpus file.
func TestCheckReportsAStubAsAStub(t *testing.T) {
	rec := &fatalRecorder{recorder{TB: t, dir: t.TempDir()}}
	steps := []Step{{Op: OpWrite, Len: 4}, {Op: OpTruncate, Off: 1}}
	Check(rec, "stub.txt", steps, func() (Target, func(), error) {
		return stubbed{newPlayed(t)}, func() {}, nil
	})
	if len(rec.errors) != 1 || !strings.HasPrefix(rec.errors[0], "stub: Truncate") {
		t.Fatalf("want the test ended with stub: Truncate, got %q", rec.errors)
	}
	if _, err := os.Stat(filepath.Join(rec.dir, "stub.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stub was written as a corpus file: %v", err)
	}
}

// deadlineRecorder is a recorder for a test that must end by deadline.
type deadlineRecorder struct {
	recorder
	deadline time.Time
}

func (r *deadlineRecorder) Deadline() (time.Time, bool) { return r.deadline, true }

// Shrinking stops short of the test's deadline, so the failing sequence is
// still written, whole if there is no time left, rather than lost when the
// test runner panics at its timeout.
func TestCheckShrinksOnlyBeforeTheTestsDeadline(t *testing.T) {
	rec := &deadlineRecorder{recorder{TB: t, dir: t.TempDir()}, time.Now().Add(shrinkMargin / 2)}
	steps := Generate(0, 200)
	Check(rec, "late.txt", steps, func() (Target, func(), error) {
		return flipTarget{newPlayed(t)}, func() {}, nil
	})
	if len(rec.errors) != 1 {
		t.Fatalf("want the divergence reported once, got %q", rec.errors)
	}
	text, err := os.ReadFile(filepath.Join(rec.dir, "late.txt"))
	ok(t, err)
	kept, err := parseCorpus(text)
	ok(t, err)
	if !slices.Equal(kept, steps) {
		t.Fatalf("wrote %d steps, want the %d that failed, unshrunk", len(kept), len(steps))
	}
}
