package segment

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestAppendParkedAtTheLoopTopWaitsARunningSeal is the deterministic guard
// for the seal wait at Append's loop top: it stages the exact state an
// append parked on the roll's gate is in when a seal takes the segment while
// it parks — the append has entered the loop, the lock is dropped, and the
// seal is past its drain with its marker read. The park hook holds the
// append at the loop top while the test drives the seal through its own
// steps, so the append's resume against the seal's closed segment is a
// certainty rather than the scheduler's guess. It catches a loop whose next
// turn reserves past a settled seal instead of rolling off the segment the
// seal closed. The take's own park — whose resume once checked the seal and
// was refused — is gone with the take; the roll that replaced it parks the
// same append the same way, and what the loop owes it is the wait, not the
// refusal, for the roll's whole point is to answer the closed segment with
// the successor it takes.
func TestAppendParkedAtTheLoopTopWaitsARunningSeal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := NewSet(64<<10, nil)
	maker := NewMaker(seam.FS(), set, 1<<62, 0, nil)
	pool := NewPool(maker, Targets{Content: 1}, nil)
	pool.Start()
	t.Cleanup(pool.Stop)
	cache := NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(8))
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	s := NewStream(1, probeIdentity, set, pool, cache, nopInternalSink())
	defer func() { _ = s.Close() }()

	// Park the appended stream's roll: the first Append runs the roll, and
	// while it is in flight a second Append parks — its resume is the
	// moment under test, so the hook stages it directly.
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	appendParkHook = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
		<-release
		appendParkHook = nil // one park: every later append runs free
	}
	defer func() { appendParkHook = nil }()

	// The first Append runs the first roll, and lands exactly once at the
	// loop top, where the hook parks it: entered, unlocked, resumable
	// against whatever takes the lock while it waits.
	first := make(chan error, 1)
	go func() {
		payload := make([]byte, 64)
		h, body := writeHeaderOf(probeFileID, payload)
		_, perr := s.Append(h, body)
		first <- perr
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the first append never reached the loop top's park")
	}

	// The parked append holds the lock nowhere — with the segment still
	// untaken, the seal cannot take it, so drive it as the seal past its
	// drain would: state sealing is what it leaves the stream in while it
	// writes its own steps.
	s.appendMu.Lock()
	s.sealState = sealSealing
	s.appendMu.Unlock()

	// The park lifts: the append resumes at the loop top, where the seal
	// wait must park it again on the drained cond, for the seal holds the
	// segment and the append has nothing in flight the seal owes a wait
	// to — so it must never move the position the seal's marker covers.
	close(release)
	select {
	case err := <-first:
		t.Fatalf("the append resumed at the loop top against a sealing stream and returned %v, want it parked until the seal settles", err)
	case <-time.After(50 * time.Millisecond):
	}

	// The wait parked the append on the drain's cond: the seal broadcasts
	// when it settles, and the append then rolls. The stage ends the seal
	// as a settled one would and lets the wait go.
	s.appendMu.Lock()
	s.sealState = sealSealed
	s.drained.Broadcast()
	s.appendMu.Unlock()
	if err := waitFor(t, first, "the append after the settled seal"); err != nil {
		t.Fatalf("the append after the settled seal: %v", err)
	}
	select {
	case <-parked:
		t.Fatalf("the append parked a second time: the one-park hook never re-arms, so a second park loops the roll")
	case <-time.After(50 * time.Millisecond):
	}
}

// hookDeadline is the bound this package's waits carry: long enough that a
// slow machine's real work always lands inside it, short enough that a hang
// the package's own timeout would swallow fails the test on its own
// assertion instead.
const hookDeadline = 10 * time.Second

// waitFor receives one value before hookDeadline, or fails t naming what
// did not happen — a parked operation that never returns shows as the named
// assertion, never a silent package timeout.
func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(hookDeadline):
		t.Fatalf("%s within %v", what, hookDeadline)
		return *new(T)
	}
}

// writeHeaderOf builds one write record of the payload given, under the
// probe identity's journal shape.
func writeHeaderOf(fid core.FileID, payload []byte) (format.RecordHeader, format.RecordBody) {
	return format.RecordHeader{
		Kind:    format.KindWrite,
		File:    fid,
		Extent:  core.Extent{Off: 0, Len: int64(len(payload))},
		Version: core.Version{Counter: 1},
		Tag:     7,
	}, format.RecordBody{Payload: payload}
}

// probeFileID is one file the probe identity's records name: a real one, so
// a record that does escape past the refusal encodes instead of panicking —
// the test then fails on its own assertion rather than a broken record.
var probeFileID = core.NewFileID([16]byte{1, 0xaa})
