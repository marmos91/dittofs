package journalv2test

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// parent is the test an attemptTB reports to, keeping what it reports.
type parent struct {
	testing.TB
	errors []string
}

func (p *parent) Errorf(format string, args ...any) {
	p.errors = append(p.errors, fmt.Sprintf(format, args...))
}

// A fixture failing inside a sequence ends the sequence with its message as
// the error, and the test goes on.
func TestAttemptFailureEndsOnlyTheSequence(t *testing.T) {
	a := &attemptTB{TB: t}
	reached := false
	err := a.guard(func() {
		a.Fatalf("fixture: init: %v", errors.New("no journal"))
		reached = true
	})
	if err == nil || err.Error() != "fixture: init: no journal" || reached {
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
	a := &attemptTB{TB: p}
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
