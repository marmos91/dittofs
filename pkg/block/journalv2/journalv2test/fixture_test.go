package journalv2test

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/sim"
)

// fatalRecorder is a testing.TB whose Fatal stops the calling goroutine and
// keeps the message instead of failing the test. Everything else reaches the
// real test, so cleanups and the profiler run as they would.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *fatalRecorder) Fatal(args ...any) {
	r.fatal = fmt.Sprint(args...)
	runtime.Goexit()
}

// TestFixtureReportsItsOwnFailures pins that a journal the fixture cannot
// set up fails the test as the fixture's failure, never as the check's.
func TestFixtureReportsItsOwnFailures(t *testing.T) {
	eio := errors.New("injected EIO")
	fault, err := sim.NewSchedule(1, nil, sim.Rule{Op: "*", Key: "*", Action: sim.FaultAction{Kind: sim.Fail, Err: eio}})
	if err != nil {
		t.Fatal(err)
	}

	r := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		newFixture(r, fault, true, FixtureSegmentCap, nil)
	}()
	<-done

	if !strings.HasPrefix(r.fatal, "fixture: ") {
		t.Fatalf("a journal whose every storage call fails was reported as %q, want a message starting %q", r.fatal, "fixture: ")
	}
}

// errorRecorder is a testing.TB that keeps what Errorf reports instead of
// failing the test.
type errorRecorder struct {
	testing.TB
	errors []string
}

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// TestAbandonLeavesTheJournalUnclosed pins that an abandoned fixture's
// cleanup calls nothing on its journal or handles, so a call blocked in them
// cannot hang it.
func TestAbandonLeavesTheJournalUnclosed(t *testing.T) {
	r := &errorRecorder{TB: t}
	f := &Fixture{tb: r, J: &journalv2.Journal{}, handles: map[int]*journalv2.Handle{0: {}}}

	f.Abandon()
	f.close()

	if len(r.errors) != 0 || len(f.handles) != 0 {
		t.Fatalf("an abandoned fixture's cleanup reported %q and kept %d handles, want it to touch nothing", r.errors, len(f.handles))
	}
}
