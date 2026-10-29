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
		newFixture(r, fault, true, FixtureSegmentCap)
	}()
	<-done

	if !strings.HasPrefix(r.fatal, "fixture: ") {
		t.Fatalf("a journal whose every storage call fails was reported as %q, want a message starting %q", r.fatal, "fixture: ")
	}
}

// The calls that may be made on a fixture with no journal in its directory,
// each checked for what it leaves behind on both of its outcomes, so the
// checks hold with the journal stubbed and once it exists.

// TestShutdownOnNoJournalIsIdle pins that shutting down a journal that is not
// open changes nothing and makes no call on the seam.
func TestShutdownOnNoJournalIsIdle(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap)
	f.Shutdown()
	f.Shutdown()

	if f.J != nil {
		t.Fatalf("a shut fixture has a journal: %v", f.J)
	}
	for _, op := range []string{OpClose, OpSync, OpSyncDir} {
		if n := f.Seam.Calls(op); n != 0 {
			t.Fatalf("shutting down with no journal made %d %s calls", n, op)
		}
	}
}

// TestTryReopenAsKeepsTheIdentity pins that an open that fails leaves the
// journal shut and the fixture's identity as it was: on an empty directory
// every open fails, whatever identity it expects.
func TestTryReopenAsKeepsTheIdentity(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap)
	for _, expect := range []journalv2.JournalID{{}, {1}} {
		if err := f.TryReopenAs(expect); err == nil {
			t.Fatalf("opening an empty directory as %x succeeded", expect)
		}
		if f.J != nil {
			t.Fatalf("a failed open left a journal: %v", f.J)
		}
		if f.ID() != (journalv2.JournalID{}) {
			t.Fatalf("a failed open as %x changed the fixture's identity to %x", expect, f.ID())
		}
	}
	if err := f.TryReopen(); err == nil || f.J != nil {
		t.Fatalf("opening an empty directory with the fixture's identity: error %v, journal %v", err, f.J)
	}
}

// TestTryInitRecordsOnSuccessOnly pins that TryInit makes its identity the
// fixture's only when Init succeeded, and opens nothing either way.
func TestTryInitRecordsOnSuccessOnly(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap)
	id, err := f.TryInit()

	switch {
	case err != nil && f.ID() != (journalv2.JournalID{}):
		t.Fatalf("Init failed (%v) and the fixture's identity is %x, want it unchanged", err, f.ID())
	case err == nil && (id == (journalv2.JournalID{}) || f.ID() != id):
		t.Fatalf("Init returned identity %x and the fixture's is %x, want the same and not zero", id, f.ID())
	}
	if f.J != nil {
		t.Fatalf("TryInit opened a journal: %v", f.J)
	}
}

// TestTryRestampLeavesTheJournalShut pins that a restamp opens nothing and,
// on an empty directory, fails as an open does.
func TestTryRestampLeavesTheJournalShut(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap)
	if err := f.TryRestamp(journalv2.InstallationID{1}); err == nil {
		t.Fatalf("restamping an empty directory succeeded")
	}
	if f.J != nil {
		t.Fatalf("TryRestamp left a journal open: %v", f.J)
	}
}

// TestTryCrashReturnsTheRefusalOrTheOpensError pins that a crash the seam
// refuses, and an open that fails after one it made, come back as errors
// with the journal shut.
func TestTryCrashReturnsTheRefusalOrTheOpensError(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap)
	file, err := f.Seam.Create("seg")
	if err != nil {
		t.Fatalf("create a file: %v", err)
	}

	// A crash keeping a write with none unsynced is refused by the seam, and
	// a refused crash changes nothing: the file created is still open.
	if err := f.TryCrash(Keeping(1)); !errors.Is(err, ErrCrashRefused) {
		t.Fatalf("TryCrash of a crash the seam refuses returned %v, want the seam's refusal", err)
	}

	if _, err := file.WriteAt([]byte("a"), 0); err != nil {
		t.Fatalf("a write after the refused crash: %v; the refusal crashed the storage", err)
	}

	if err := f.TryCrash(); err == nil || f.J != nil {
		t.Fatalf("opening an empty directory after a crash: error %v, journal %v", err, f.J)
	}
}

// TestTryAbandonReturnsTheOpensError pins that an open that fails after the
// process was abandoned comes back as an error with the journal shut.
func TestTryAbandonReturnsTheOpensError(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap)
	if err := f.TryAbandon(); err == nil || f.J != nil {
		t.Fatalf("opening an empty directory after an abandon: error %v, journal %v", err, f.J)
	}
}
