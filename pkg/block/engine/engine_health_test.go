package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeRemoteStore wraps a memory remote store with a controllable health check.
// When healthy is false, both HealthCheck and Healthcheck simulate an outage.
// All other methods delegate to the wrapped store.
//
// Both probes are overridden because RemoteStore now requires the
// lowercase-c Healthcheck (returning health.Report) alongside the
// legacy capital-C HealthCheck. Without overriding both, Go's interface
// embedding would silently dispatch Healthcheck to the underlying
// memory store, ignoring this fake's `healthy` flag — making any test
// that simulates an outage via the new path a false positive.
type fakeRemoteStore struct {
	remote.RemoteStore
	healthy atomic.Bool
}

func newFakeRemoteStore() *fakeRemoteStore {
	f := &fakeRemoteStore{RemoteStore: remotememory.New()}
	f.healthy.Store(true)
	return f
}

func (f *fakeRemoteStore) HealthCheck(ctx context.Context) error {
	if !f.healthy.Load() {
		return errors.New("simulated outage")
	}
	return f.RemoteStore.HealthCheck(ctx)
}

func (f *fakeRemoteStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if !f.healthy.Load() {
		return health.NewUnhealthyReport("simulated outage", time.Since(start))
	}
	return f.RemoteStore.Healthcheck(ctx)
}

func (f *fakeRemoteStore) SetHealthy(h bool) { f.healthy.Store(h) }

// newHealthTestEngine creates an engine.Store with an FSStore
// local store, a controllable fake remote store, and a syncer with
// short health intervals. The FSStore is constructed with an inline
// RollupStore + a tight stabilization window so the
// AppendWrite → rollup → CAS chunk → FileBlock-row pipeline runs
// promptly inside the test.
func buildHealthTestEngine(t *testing.T) (*Store, *fakeRemoteStore) {
	t.Helper()

	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(tmpDir, 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 50,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions() error = %v", err)
	}
	// Bring up the rollup worker pool so AppendWrite-staged bytes get
	// chunked into CAS objects deterministically inside the test.
	if err := localStore.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}

	fakeRemote := newFakeRemoteStore()

	syncCfg := SyncerConfig{
		ParallelUploads:             4,
		ParallelDownloads:           4,
		PrefetchBlocks:              0,
		UploadInterval:              50 * time.Millisecond,
		UploadDelay:                 0,
		HealthCheckInterval:         20 * time.Millisecond,
		HealthCheckFailureThreshold: 2,
		UnhealthyCheckInterval:      10 * time.Millisecond,
	}

	syncer := NewSyncer(localStore, fakeRemote, ms, syncCfg)

	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          fakeRemote,
		Syncer:          syncer,
		FileBlockStore:  ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("engine.New() error = %v", err)
	}

	return bs, fakeRemote
}

// newHealthTestEngine builds and starts a health test engine with the remote
// initially healthy.
func newHealthTestEngine(t *testing.T) (*Store, *fakeRemoteStore) {
	t.Helper()
	bs, fakeRemote := buildHealthTestEngine(t)
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, fakeRemote
}

// TestEngineHealthEvictionSuspended_RemoteUnhealthyAtStart is a regression test
// for the health-callback ordering bug: the callback must be wired BEFORE the
// syncer starts, otherwise the initial "unhealthy" transition fired by the
// startup probe is lost and eviction stays enabled while the remote is down —
// risking eviction of not-yet-mirrored local CAS chunks. With the remote
// unhealthy from the outset there is no later transition to recover the lost
// signal, so the only thing keeping eviction suspended is correct startup
// wiring + reconciliation.
func TestEngineHealthEvictionSuspended_RemoteUnhealthyAtStart(t *testing.T) {
	bs, fakeRemote := buildHealthTestEngine(t)
	// Remote is unhealthy BEFORE Start, so the initial transition fires during
	// startup (the window the old code dropped).
	fakeRemote.SetHealthy(false)

	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	// Allow the startup probe + threshold to settle, then assert eviction is
	// suspended. No further health transition occurs (remote stays down), so a
	// pass here proves the startup signal was not dropped.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if bs.GetStats().EvictionSuspended {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stats := bs.GetStats()
	if stats.RemoteHealthy {
		t.Fatal("expected remote unhealthy at start")
	}
	if !stats.EvictionSuspended {
		t.Fatal("expected eviction suspended when remote is unhealthy at start (health callback wired before syncer start)")
	}
}

// TestEngineHealthEvictionSuspension verifies that when remote goes unhealthy
// the engine's health callback disables eviction on the local store. When remote
// recovers, eviction is re-enabled.
func TestEngineHealthEvictionSuspension(t *testing.T) {
	bs, fakeRemote := newHealthTestEngine(t)

	// Initially, eviction should be enabled and remote should be healthy.
	stats := bs.GetStats()
	if stats.EvictionSuspended {
		t.Fatal("expected eviction NOT suspended initially")
	}
	if !stats.RemoteHealthy {
		t.Fatal("expected remote healthy initially")
	}

	// Simulate outage.
	fakeRemote.SetHealthy(false)

	// Wait for health monitor to detect failure (threshold=2 at 20ms interval).
	time.Sleep(150 * time.Millisecond)

	stats = bs.GetStats()
	if stats.RemoteHealthy {
		t.Fatal("expected remote unhealthy after simulated outage")
	}
	if !stats.EvictionSuspended {
		t.Fatal("expected eviction suspended during outage")
	}

	// Restore health.
	fakeRemote.SetHealthy(true)

	// Wait for recovery.
	time.Sleep(100 * time.Millisecond)

	stats = bs.GetStats()
	if !stats.RemoteHealthy {
		t.Fatal("expected remote healthy after recovery")
	}
	if stats.EvictionSuspended {
		t.Fatal("expected eviction re-enabled after recovery")
	}
}

// TestEngineBlockStoreStatsHealthFields verifies BlockStoreStats includes correct
// remote_healthy, eviction_suspended, and outage_duration_seconds values
// in both healthy and unhealthy states.
func TestEngineBlockStoreStatsHealthFields(t *testing.T) {
	bs, fakeRemote := newHealthTestEngine(t)

	// Healthy state.
	stats := bs.GetStats()
	if !stats.RemoteHealthy {
		t.Fatal("expected RemoteHealthy == true in healthy state")
	}
	if stats.EvictionSuspended {
		t.Fatal("expected EvictionSuspended == false in healthy state")
	}
	if stats.OutageDurationSecs != 0 {
		t.Fatalf("expected OutageDurationSecs == 0 in healthy state, got %f", stats.OutageDurationSecs)
	}

	// Simulate outage.
	fakeRemote.SetHealthy(false)
	time.Sleep(150 * time.Millisecond)

	stats = bs.GetStats()
	if stats.RemoteHealthy {
		t.Fatal("expected RemoteHealthy == false during outage")
	}
	if !stats.EvictionSuspended {
		t.Fatal("expected EvictionSuspended == true during outage")
	}
	if stats.OutageDurationSecs <= 0 {
		t.Fatalf("expected OutageDurationSecs > 0 during outage, got %f", stats.OutageDurationSecs)
	}

	// Restore health.
	fakeRemote.SetHealthy(true)
	time.Sleep(100 * time.Millisecond)

	stats = bs.GetStats()
	if !stats.RemoteHealthy {
		t.Fatal("expected RemoteHealthy == true after recovery")
	}
	if stats.OutageDurationSecs != 0 {
		t.Fatalf("expected OutageDurationSecs == 0 after recovery, got %f", stats.OutageDurationSecs)
	}
}

// Compile-time check that fakeRemoteStore satisfies remote.RemoteStore.
var _ remote.RemoteStore = (*fakeRemoteStore)(nil)
