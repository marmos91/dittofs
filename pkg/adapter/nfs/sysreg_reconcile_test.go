package nfs

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter/sidecar"
)

// newSysregAdapter returns an adapter whose sysreg sidecar talks to addr, with
// the sidecar group seeded so Reconcile is live.
func newSysregAdapter(addr string) (*NFSAdapter, func(bool)) {
	a := &NFSAdapter{sidecars: sidecar.NewGroup(), sysregAddr: addr}
	a.sidecars.SetBaseContext(context.Background())

	// Mutate the way applyNFSSettings does: a fresh pointer, assigned under
	// configMu. Taking the address once and flipping the bool through it would
	// race the sidecar goroutine's read — configMu guards the config FIELD, not
	// the bool the field points at, so an aliased write escapes it entirely.
	return a, func(v bool) {
		a.configMu.Lock()
		defer a.configMu.Unlock()
		a.config.Portmapper.RegisterWithSystem = &v
	}
}

// Toggling the register-with-system setting starts and stops the sysreg sidecar
// on a running adapter, without a restart.
func TestReconcileSysregTogglesSidecar(t *testing.T) {
	// Dead address: registration finds no portmapper and gives up immediately.
	a, setRegisterWithSystem := newSysregAdapter("127.0.0.1:1")

	a.reconcileSysreg()
	waitSysreg(t, a, false, "register-with-system unset")

	setRegisterWithSystem(true)
	a.reconcileSysreg()
	waitSysreg(t, a, true, "register-with-system enabled")

	setRegisterWithSystem(false)
	a.reconcileSysreg()
	waitSysreg(t, a, false, "register-with-system disabled")
}

// A flip that lands while a transition is still talking to rpcbind is applied
// rather than dropped. A caller that reacts to the sidecar's running state
// always issues its flip into an in-flight transition (see waitRunning), so the
// window this covers is the normal case, not a rare one. The registration is
// held open until the flip has been issued, so the race is arranged rather than
// waited for.
func TestReconcileSysregAppliesFlipDuringTransition(t *testing.T) {
	addr, release := heldSysregAddr(t)
	a, setRegisterWithSystem := newSysregAdapter(addr)

	setRegisterWithSystem(true)
	a.reconcileSysreg()
	waitRunning(t, a, true)
	if got := a.sysregState.Load(); got != sysregRunning {
		t.Fatalf("enable transition not in flight (state %d); the flip below would not race it", got)
	}

	setRegisterWithSystem(false)
	a.reconcileSysreg()
	release()
	waitSysreg(t, a, false, "register-with-system disabled during an in-flight enable")
}

// heldSysregAddr returns the address of a listener that accepts a connection
// and holds it open until release is called, so a sysreg registration against
// it stays in flight for exactly as long as the caller wants. Bounding the
// window by a duration instead would make every assertion about the in-flight
// state a bet on the test goroutine being scheduled inside it.
func heldSysregAddr(t *testing.T) (addr string, release func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	held := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				<-held
				_ = c.Close()
			}()
		}
	}()
	release = sync.OnceFunc(func() { close(held) })
	t.Cleanup(func() {
		release()
		_ = ln.Close()
	})
	return ln.Addr().String(), release
}

// waitRunning waits only for the sidecar's running state, which the group
// publishes when it reserves the name — before the service's Start has run.
func waitRunning(t *testing.T, a *NFSAdapter, want bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if a.sidecars.IsRunning(sysregSidecarName) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("sysreg sidecar never reached running=%v", want)
}

// waitSysreg waits for the reconciler to SETTLE on want, which is two facts,
// not one: no transition is still in flight, and the sidecar's running state
// matches. Waiting on the running state alone (waitRunning) would pass while a
// transition is mid-flight and could still move it.
func waitSysreg(t *testing.T, a *NFSAdapter, want bool, cond string) {
	t.Helper()
	var running bool
	var state int32
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		// Idle is read FIRST, for the reason reconcileSysreg states: a transition
		// holds the state non-idle until after its mutation is visible, so reading
		// the running state first can pair a pre-mutation value with the idle the
		// same transition published after settling.
		state, running = a.sysregState.Load(), a.sidecars.IsRunning(sysregSidecarName)
		if running == want && state == sysregIdle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("sysreg sidecar never settled on running=%v with %s: running=%v, reconcile state=%d",
		want, cond, running, state)
}

// After the group is torn down, a reconcile claims nothing. The adapter stops
// its sidecars before closing the listener, so the accept loop keeps calling
// reconcileSysreg during teardown with the setting still enabled — a state the
// steady-state check can never match again, because StopAll empties the running
// set. Claiming there would spawn a transition per connection that Reconcile
// can only no-op.
func TestReconcileSysregNoOpsAfterGroupTeardown(t *testing.T) {
	a, setRegisterWithSystem := newSysregAdapter("127.0.0.1:1")
	setRegisterWithSystem(true)
	a.reconcileSysreg()
	waitSysreg(t, a, true, "register-with-system enabled")

	if err := a.sidecars.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}

	for range 100 {
		a.reconcileSysreg()
	}
	if got := a.sysregState.Load(); got != sysregIdle {
		t.Fatalf("reconcile claimed a transition against a torn-down group: state %d", got)
	}
}
