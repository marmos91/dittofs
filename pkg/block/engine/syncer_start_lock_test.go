package engine

import (
	"context"
	"testing"
	"time"

	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// probeHookRemote runs an arbitrary function in place of the remote's health
// check, so a test can observe what the caller holds while the probe runs.
type probeHookRemote struct {
	remote.RemoteStore
	probe func(ctx context.Context) error
}

func (r *probeHookRemote) HealthCheck(ctx context.Context) error { return r.probe(ctx) }

// The eager health probe is a network round trip. Start must not hold the
// syncer lock across it, or every read and flush stalls for a remote timeout
// while a share comes up.
func TestSyncerStart_EagerProbeDoesNotHoldLock(t *testing.T) {
	var m *Syncer
	rem := &probeHookRemote{
		RemoteStore: remotememory.New(),
		probe: func(context.Context) error {
			// Takes the syncer lock: deadlocks if Start still holds it.
			m.SetHealthCallback(nil)
			return nil
		},
	}
	m = NewSyncer(memorylocal.New(), rem, newStubFileChunkStore(), DefaultConfig())

	done := make(chan struct{})
	go func() {
		m.Start(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Syncer.Start held its lock across the eager health probe")
	}
	_ = m.Close()
}
