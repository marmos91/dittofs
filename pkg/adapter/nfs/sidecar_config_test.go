package nfs

import (
	"sync"
	"testing"
)

// applySidecarConfig writes the four sidecar config fields exactly as the
// fixed applyNFSSettings does (settings.go:81-99): pointers prepared outside
// configMu, the Port read-decide-write and all four fields stored under one
// critical section. newPort <= 0 mirrors the DB fallback that keeps the
// previously applied port. Extracted here because the real function needs a
// *runtime.Runtime; the races under test are between these writes and the
// readers, not between settings sources.
func applySidecarConfig(a *NFSAdapter, enabled, registerWithSystem, udpEnabled bool, newPort int) {
	var port int
	a.configMu.Lock()
	port = a.config.Portmapper.Port
	if newPort > 0 {
		port = newPort
	}
	a.config.Portmapper.Enabled = &enabled
	a.config.Portmapper.Port = port
	a.config.Portmapper.RegisterWithSystem = &registerWithSystem
	a.config.UDP.Enabled = &udpEnabled
	a.configMu.Unlock()
}

// The sidecar config fields (Portmapper.Enabled/Port/RegisterWithSystem,
// UDP.Enabled) are written by applyNFSSettings on the settings-watcher
// goroutine, on the accept loop per connection, and at startup — three
// concurrent writers — and read by the sysreg transition goroutine
// (registerWithSystemEnabled, isUDPEnabled) and the portmapper start path
// (isPortmapperEnabled, Portmapper.Port). With -race this fails on any
// unsynchronized concurrent write/read of the plain *bool/int fields.
func TestSidecarConfigConcurrentApplyRead(t *testing.T) {
	a := &NFSAdapter{}

	const iterations = 2000
	var wg sync.WaitGroup
	wg.Add(3)

	// Two concurrent writers, mirroring the settings poller and the accept
	// loop both calling applyNFSSettings. Writer 0 alternates a zero port to
	// exercise the keep-previous-port fallback; writer 1 always sets one.
	for writer := 0; writer < 2; writer++ {
		go func(writer int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				port := 10111 + i
				if writer == 0 && i%2 == 0 {
					port = 0
				}
				applySidecarConfig(a, writer == 0, i%2 == 0, i%3 == 0, port)
			}
		}(writer)
	}

	// One reader spinning the exact reads the sysreg transition and the
	// portmapper start path perform (the production snapshotSidecarConfig
	// helper, so removing configMu from startPortmapper's or
	// systemRegMappings's snapshots turns this hammer -race red).
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = a.isPortmapperEnabled()
			_, _, _ = a.snapshotSidecarConfig()
			_ = a.registerWithSystemEnabled()
		}
	}()

	wg.Wait()
}
