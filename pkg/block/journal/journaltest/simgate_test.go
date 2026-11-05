package journaltest

import (
	"testing"
	"time"
)

// gateSegNames matches the segment syncs a gate parks in the protocol and
// choreography tests: any call on a name ending in .seg.
func gateSegNames(op string, key []byte) bool {
	return op == OpSync && hasSegSuffix(key)
}

func hasSegSuffix(key []byte) bool {
	return len(key) >= 4 && string(key[len(key)-4:]) == ".seg"
}

// waitFor parks the test until cond holds, or fails it after the bound.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !cond() {
		t.Fatalf("%s within 2s", what)
	}
}

// TestSimGateFreesExactlyOneParkPerRelease drives the gate's protocol with
// fake callers: a parked call consumes exactly one release as its own, the
// caller still parked after the release stays parked, and a released caller
// that matches again parks afresh rather than riding the same release twice.
func TestSimGateFreesExactlyOneParkPerRelease(t *testing.T) {
	g := &SimGate{Match: gateSegNames}
	g.Arm()

	returned := make(chan int, 2)
	synced := func() {
		_, _, _ = g.At(OpSync, []byte("0.seg"))
		returned <- 1
	}
	go synced()
	go synced()

	// Both callers park and announce one arrival each.
	waitFor(t, "two matched calls did not both park", func() bool { return g.Parked() == 2 })
	for range 2 {
		select {
		case <-g.Arrivals:
		case <-time.After(2 * time.Second):
			t.Fatal("a parked call did not announce its arrival")
		}
	}
	select {
	case <-returned:
		t.Fatal("a parked call returned with no release")
	default:
	}

	// One release frees one caller; the other stays parked.
	g.Release()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the released call did not return within 2s")
	}
	waitFor(t, "one release of two parked calls did not leave one parked", func() bool { return g.Parked() == 1 })

	// The freed caller that matches again parks afresh: a second call
	// announces a fresh arrival and waits for its own release, not a
	// leftover one.
	go synced()
	waitFor(t, "the freed caller did not park afresh", func() bool { return g.Parked() == 2 })
	select {
	case <-g.Arrivals:
	case <-time.After(2 * time.Second):
		t.Fatal("the caller parking afresh did not announce a fresh arrival")
	}
	select {
	case <-returned:
		t.Fatal("a second call returned on the first release's banked turn")
	default:
	}
	g.Release()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the second released call did not return within 2s")
	}

	// A disarm frees everything parked, and a later matched call passes.
	g.Disarm()
	waitFor(t, "a disarmed gate still held calls parked", func() bool { return g.Parked() == 0 })
	done := make(chan struct{})
	go func() { _, _, _ = g.At(OpSync, []byte("0.seg")); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a matched call after the disarm parked")
	}
}

// TestSimGatePassesUnmatchedCalls checks the match is the only thing that
// parks: a call the matcher answers false passes with the gate armed.
func TestSimGatePassesUnmatchedCalls(t *testing.T) {
	g := &SimGate{Match: gateSegNames}
	g.Arm()
	defer g.Disarm()
	done := make(chan struct{})
	go func() { _, _, _ = g.At(OpWrite, []byte("0.seg")); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an unmatched call parked at the armed gate")
	}
	if g.Parked() != 0 {
		t.Fatalf("an unmatched call parked %d callers, want none", g.Parked())
	}
}

// TestSimGateCountsDroppedArrivals checks the gate announces a park it
// cannot queue neither silently nor forever: with the buffer already full,
// the announcement is dropped and counted, and the caller still waits for
// its own release rather than blocking every call of the gate.
func TestSimGateCountsDroppedArrivals(t *testing.T) {
	g := &SimGate{Match: gateSegNames}
	g.Arm()
	defer g.Disarm()

	// Take every slot of the arrival buffer, as more parked calls than it
	// holds would: the buffer is the gate's only queue.
	for range arrivalsMax {
		g.Arrivals <- struct{}{}
	}

	returned := make(chan struct{})
	go func() { _, _, _ = g.At(OpSync, []byte("0.seg")); close(returned) }()
	waitFor(t, "the call did not park behind the full buffer", func() bool { return g.Parked() == 1 })
	// The park is observable before its announcement is — the park counts
	// itself, then announces — so the dropped count is waited for too, not
	// assumed to have landed the moment the park did.
	waitFor(t, "the park behind the full buffer did not drop its announcement", func() bool { return g.Dropped() == 1 })
	if g.Dropped() != 1 {
		t.Fatalf("a park behind the full arrival buffer dropped %d announcements, want 1", g.Dropped())
	}
	select {
	case <-returned:
		t.Fatal("the parked call returned with no release")
	default:
	}

	g.Release()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the call dropped from the buffer did not take its release")
	}
	if g.Dropped() != 1 {
		t.Fatalf("a released call changed the dropped count to %d", g.Dropped())
	}
}

// TestSimGateArmStartsWithoutBankedTurns pins that a release arriving while
// the gate is disarmed answers no call of the next window: the drain loop
// after a Disarm releases every arrival still queued, and a turn banked by
// one of those releases would let the next window's first matched call pass
// with no park and no arrival, riding a turn nobody asked for it.
func TestSimGateArmStartsWithoutBankedTurns(t *testing.T) {
	g := &SimGate{Match: gateSegNames}

	// Park one call, take its arrival, and free it through a disarm — the
	// shape of the protocol's drain path.
	g.Arm()
	returned := make(chan struct{})
	go func() { _, _, _ = g.At(OpSync, []byte("0.seg")); close(returned) }()
	waitFor(t, "the call did not park", func() bool { return g.Parked() == 1 })
	select {
	case <-g.Arrivals:
	case <-time.After(2 * time.Second):
		t.Fatal("the parked call did not announce")
	}
	g.Disarm()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the disarm did not free the parked call")
	}

	// A release on the disarmed gate — one the drained queue would bank.
	if g.Armed() {
		t.Fatal("the gate reports itself armed after a disarm")
	}
	g.Release()

	// The next window opens: its first matched call must park and announce,
	// not ride the turn banked between the disarm and the arm.
	g.Arm()
	done := make(chan struct{})
	go func() { _, _, _ = g.At(OpSync, []byte("0.seg")); close(done) }()
	select {
	case <-g.Arrivals:
	case <-done:
		t.Fatal("the first matched call of a fresh window passed with no park")
	case <-time.After(2 * time.Second):
		t.Fatal("the first matched call of a fresh window neither parked nor passed")
	}
	select {
	case <-done:
		t.Fatal("the first matched call of a fresh window rode a banked turn")
	default:
	}
	g.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the parked call did not take its release")
	}
}
