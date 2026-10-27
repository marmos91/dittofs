package journalv2test

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

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
		newFixture(r, fault, true)
	}()
	<-done

	if !strings.HasPrefix(r.fatal, "fixture: ") {
		t.Fatalf("a journal whose every storage call fails was reported as %q, want a message starting %q", r.fatal, "fixture: ")
	}
}
