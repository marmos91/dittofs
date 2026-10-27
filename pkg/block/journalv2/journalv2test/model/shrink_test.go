package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

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
	got := Shrink(steps, fails)
	want := []Step{{Op: OpDelete, File: 2}, {Op: OpTruncate, File: 2, Off: 9}}
	if !slices.Equal(got, want) {
		t.Fatalf("shrunk to\n%s\nwant\n%s", FormatCorpus(got), FormatCorpus(want))
	}
}

// Shrinking also shortens the extents of the steps it keeps.
func TestShrinkShortensExtents(t *testing.T) {
	fails := func(c []Step) bool {
		return slices.ContainsFunc(c, func(s Step) bool { return s.Op == OpWrite && s.Len >= 3 })
	}
	got := Shrink([]Step{{Op: OpWrite, Len: 1000}}, fails)
	if len(got) != 1 || got[0].Len >= 6 {
		t.Fatalf("shrunk to\n%s\nwant one write shorter than 6 bytes", FormatCorpus(got))
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

// flipView serves file 1's byte 7 with its low bit flipped whenever it is
// held: a journal returning one wrong byte.
type flipView struct{ View }

func (v flipView) ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	n, missing, held, asOf, err := v.View.ReadAt(ctx, id, off, p)
	const at = 7
	if id == FileOf(1) && off <= at && at-off < int64(n) &&
		!slices.ContainsFunc(missing, func(e journalv2.Extent) bool { return covers(e, at) }) {
		p[at-off] ^= 1
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
	shrunk, err := ParseCorpus(text)
	ok(t, err)
	if len(shrunk) > 3 {
		t.Fatalf("shrunk to %d steps, want at most 3:\n%s", len(shrunk), text)
	}
	if _, err := Run(t.Context(), flipTarget{newPlayed(t)}, shrunk); !errors.Is(err, ErrDiverged) {
		t.Fatalf("the corpus file replayed with %v, want a divergence", err)
	}
	if _, err := Run(t.Context(), newPlayed(t), shrunk); err != nil {
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
	if len(rec.errors) != 1 || !errors.Is(firstErr(t, steps), ErrMisuse) {
		t.Fatalf("want the misuse reported, got %q", rec.errors)
	}
	if _, err := os.Stat(filepath.Join(rec.dir, "misuse.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a misuse was written as a corpus file: %v", err)
	}
}

func firstErr(t *testing.T, steps []Step) error {
	_, err := Run(t.Context(), lowFloor{newPlayed(t)}, steps)
	return err
}
