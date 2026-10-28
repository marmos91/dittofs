package journalv2test

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/model"
)

// parent is the test an attemptTB reports to, keeping what it reports.
type parent struct {
	testing.TB
	errors []string
}

func (p *parent) Errorf(format string, args ...any) {
	p.errors = append(p.errors, fmt.Sprintf(format, args...))
}

var errNoJournal = errors.New("no journal")

// A fixture failing inside a sequence ends the sequence with its message as
// the error, and the test goes on.
func TestAttemptFailureEndsOnlyTheSequence(t *testing.T) {
	a := &attemptTB{TB: t, parent: t.TempDir()}
	reached := false
	err := a.guard(func() {
		a.Fatalf("fixture: init: %v", errNoJournal)
		reached = true
	})
	if err == nil || err.Error() != "fixture: init: no journal" || !errors.Is(err, errNoJournal) || reached {
		t.Fatalf("guard returned %v, ran past the failure %t", err, reached)
	}
	if err := a.guard(func() {}); err != nil {
		t.Fatalf("a sequence that reports nothing returned %v", err)
	}
}

// What a sequence opened is closed when it ends, last opened first, and its
// directory removed; a failure while closing reaches the test.
func TestAttemptCleanupsRunWhenTheSequenceEnds(t *testing.T) {
	p := &parent{TB: t}
	a := &attemptTB{TB: p, parent: t.TempDir()}
	dir := a.TempDir()
	var order []int
	a.Cleanup(func() { order = append(order, 1) })
	a.Cleanup(func() { a.Errorf("fixture: close: %v", "stuck") })
	a.Cleanup(func() { order = append(order, 3) })

	a.finish()

	if !slices.Equal(order, []int{3, 1}) {
		t.Fatalf("cleanups ran in order %v, want [3 1]", order)
	}
	if len(p.errors) != 1 || !strings.Contains(p.errors[0], "fixture: close: stuck") {
		t.Fatalf("the test was told %q", p.errors)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the sequence's directory is still there: %v", err)
	}
}

// A journal that does not open after a crash is the journal's failure, which
// the model reports as a divergence; a crash that could not be made is not.
func TestAFailedOpenIsAReopenError(t *testing.T) {
	a := &attemptTB{TB: t, parent: t.TempDir()}
	open := reopened(a.guard(func() { a.Fatalf("fixture: open: %v", errNoJournal) }))
	var re model.ReopenError
	if !errors.As(open, &re) || !errors.Is(open, errNoJournal) {
		t.Fatalf("a failed open returned %v", open)
	}
	for _, msg := range []string{"fixture: attach share %d: %v", "fixture: close a handle: %d %v"} {
		err := reopened(a.guard(func() { a.Fatalf(msg, 1, errNoJournal) }))
		if !errors.As(err, &re) {
			t.Fatalf("%q returned %v, want a reopen error", msg, err)
		}
	}
	crash := reopened(a.guard(func() { a.Fatalf("fixture: crash: %v", errNoJournal) }))
	if errors.As(crash, &re) || crash == nil {
		t.Fatalf("a failed crash returned %v", crash)
	}
}
