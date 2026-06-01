package nfs

import (
	"testing"
	"time"

	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
)

// newConfirmedSM returns a StateManager with one confirmed v4 client, so
// GetConfirmedClientIDs is non-empty and the coordinator's StartGracePeriod
// actually activates v4 grace (an empty client set is skipped by design).
func newConfirmedSM(t *testing.T) *v4state.StateManager {
	t.Helper()
	sm := v4state.NewStateManager(time.Hour)
	verf := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	res, err := sm.SetClientID("client-A", verf, v4state.CallbackInfo{}, "10.0.0.1:1", "uid:1000")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := sm.ConfirmClientID(res.ClientID, res.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	return sm
}

// TestGraceCoordinator_RefcountKeepsV4GraceUntilLastShare is the REVIEW slice-3
// regression. N shares enter lock-manager grace and couple through the
// coordinator. Removing/ending ONE share must NOT lift global v4 grace while
// other shares' windows are still open; v4 grace lifts only when the LAST
// share's window closes (refcount zero). Under the old "first in, first out"
// policy a single OnLockGraceEnd force-ended v4 grace outright.
func TestGraceCoordinator_RefcountKeepsV4GraceUntilLastShare(t *testing.T) {
	sm := newConfirmedSM(t)
	c := &nfsGraceCoordinator{sm: sm}

	// Three shares enter lock-manager grace.
	c.OnLockGraceStart([]string{"nlm-1"})
	if !sm.IsInGrace() {
		t.Fatal("first share's grace start must couple v4 into grace")
	}
	c.OnLockGraceStart([]string{"nlm-2"})
	c.OnLockGraceStart([]string{"nlm-3"})
	if !sm.IsInGrace() {
		t.Fatal("v4 grace must remain active across multiple coupled shares")
	}

	// Remove the first two shares (early lock-manager grace-end, e.g. via
	// RemoveStoreForShare or per-share reclaim). v4 grace must stay up.
	c.OnLockGraceEnd()
	if !sm.IsInGrace() {
		t.Fatal("v4 grace lifted after ONE share ended while others outstanding (premature lift)")
	}
	c.OnLockGraceEnd()
	if !sm.IsInGrace() {
		t.Fatal("v4 grace lifted while one share still in grace (premature lift)")
	}

	// Last share ends: refcount hits zero and this coordinator OWNS v4 grace
	// (it started it), so v4 grace lifts now.
	c.OnLockGraceEnd()
	if sm.IsInGrace() {
		t.Fatal("v4 grace must lift once the LAST coupled share's window closes")
	}
}

// TestGraceCoordinator_BootSeededRosterNotForceEnded asserts that when v4 grace
// was independently boot-seeded (LoadClientRecovery roster, slice-2), the
// coordinator does NOT force-end it at refcount zero. The v4 machine governs its
// own lift via its reclaim early-exit and hard timer, so an early
// lock-manager grace-end can never prematurely bypass the v4 reclaim roster.
func TestGraceCoordinator_BootSeededRosterNotForceEnded(t *testing.T) {
	sm := v4state.NewStateManager(time.Hour)
	// Simulate LoadClientRecovery seeding v4 grace with a durable roster BEFORE
	// any coordinator coupling (boot order: recovery load, then adapter wires
	// the coordinator and catches up shares already in grace).
	sm.StartGracePeriod([]uint64{42})
	if !sm.IsInGrace() {
		t.Fatal("precondition: boot-seeded v4 grace must be active")
	}

	c := &nfsGraceCoordinator{sm: sm}

	// Adapter catch-up: two shares already in lock-manager grace couple in.
	c.OnLockGraceStart([]string{"nlm-1"})
	c.OnLockGraceStart([]string{"nlm-2"})

	// Both shares' lock-manager grace windows end. Even at refcount zero, the
	// coordinator must DEFER to the v4 machine (it did not start it), so v4 grace
	// stays up under its own roster/timer governance.
	c.OnLockGraceEnd()
	c.OnLockGraceEnd()
	if !sm.IsInGrace() {
		t.Fatal("coordinator force-ended boot-seeded v4 grace at refcount zero (must defer to v4 timer/roster)")
	}

	// The v4 machine's own hard timer remains the backstop, so v4 grace can be
	// ended through its own path without wedging.
	sm.ForceEndGrace()
	if sm.IsInGrace() {
		t.Fatal("v4 machine's own ForceEndGrace must still lift grace")
	}
}

// TestGraceCoordinator_EndUnderflowIgnored guards against refcount underflow: an
// unbalanced OnLockGraceEnd (more ends than starts) must be ignored, never
// driving the count negative or panicking.
func TestGraceCoordinator_EndUnderflowIgnored(t *testing.T) {
	sm := newConfirmedSM(t)
	c := &nfsGraceCoordinator{sm: sm}

	// End with no outstanding starts: no-op, no panic.
	c.OnLockGraceEnd()
	c.OnLockGraceEnd()

	// A subsequent start/end cycle must still behave correctly (count not stuck
	// negative).
	c.OnLockGraceStart([]string{"nlm-1"})
	if !sm.IsInGrace() {
		t.Fatal("start after spurious ends must still couple v4 grace")
	}
	c.OnLockGraceEnd()
	if sm.IsInGrace() {
		t.Fatal("balanced end after start must lift coordinator-owned v4 grace")
	}
}

// TestGraceCoordinator_NilStateManagerNoPanic confirms the coordinator is a
// safe no-op when no StateManager is wired (defensive).
func TestGraceCoordinator_NilStateManagerNoPanic(t *testing.T) {
	c := &nfsGraceCoordinator{sm: nil}
	c.OnLockGraceStart([]string{"x"})
	c.OnLockGraceEnd()
}
