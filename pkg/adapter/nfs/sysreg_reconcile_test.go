package nfs

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter/auxsvc"
)

// Toggling the register-with-system setting must start and stop the sysreg
// sidecar on a running adapter, without a restart.
func TestReconcileSysregTogglesSidecar(t *testing.T) {
	// Dead address: registration finds no portmapper and gives up immediately.
	a := &NFSAdapter{sidecars: auxsvc.NewGroup(), sysregAddr: "127.0.0.1:1"}
	a.sidecars.SetBaseContext(context.Background())

	// Mutate the way applyNFSSettings does: a fresh pointer, assigned under
	// configMu. Taking the address once and flipping the bool through it races
	// the sidecar goroutine's read — configMu guards the config FIELD, not the
	// bool the field points at, so an aliased write escapes it entirely.
	setRegisterWithSystem := func(v bool) {
		a.configMu.Lock()
		defer a.configMu.Unlock()
		a.config.Portmapper.RegisterWithSystem = &v
	}

	a.reconcileSysreg()
	waitSysreg(t, a, false, "sysreg sidecar running while register-with-system is unset")

	setRegisterWithSystem(true)
	a.reconcileSysreg()
	waitSysreg(t, a, true, "sysreg sidecar not started after enabling register-with-system")

	setRegisterWithSystem(false)
	a.reconcileSysreg()
	waitSysreg(t, a, false, "sysreg sidecar still running after disabling register-with-system")
}

// waitSysreg waits for the sysreg sidecar to reach want; the reconcile applies
// the transition in the background.
func waitSysreg(t *testing.T, a *NFSAdapter, want bool, msg string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if a.sidecars.IsRunning(sysregSidecarName) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
