package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	blocksync "github.com/marmos91/dittofs/pkg/blockstore/sync"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeRemoteStore wraps a memory remote store with a controllable health check.
// When healthy is false, HealthCheck returns an error. All other methods
// delegate to the wrapped store.
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

func (f *fakeRemoteStore) SetHealthy(h bool) { f.healthy.Store(h) }

// newHealthTestEngine creates an engine.BlockStore with an FSStore local cache,
// a controllable fake remote store, and a syncer with short health intervals.
func newHealthTestEngine(t *testing.T) (*BlockStore, *fakeRemoteStore) {
	t.Helper()

	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.New(tmpDir, 100*1024*1024, 16*1024*1024, ms)
	if err != nil {
		t.Fatalf("fs.New() error = %v", err)
	}

	fakeRemote := newFakeRemoteStore()

	syncCfg := blocksync.Config{
		ParallelUploads:             4,
		ParallelDownloads:           4,
		PrefetchBlocks:              0,
		UploadInterval:              50 * time.Millisecond,
		UploadDelay:                 0,
		HealthCheckInterval:         20 * time.Millisecond,
		HealthCheckFailureThreshold: 2,
		UnhealthyCheckInterval:      10 * time.Millisecond,
	}

	syncer := blocksync.New(localStore, fakeRemote, ms, syncCfg)

	bs, err := New(Config{
		Local:  localStore,
		Remote: fakeRemote,
		Syncer: syncer,
	})
	if err != nil {
		t.Fatalf("engine.New() error = %v", err)
	}

	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start() error = %v", err)
	}

	t.Cleanup(func() { _ = bs.Close() })

	return bs, fakeRemote
}

// TestEngineHealthEvictionSuspension verifies that when remote goes unhealthy,
// the engine's health callback disables eviction on the local store. When remote
// recovers, eviction is re-enabled.
func TestEngineHealthEvictionSuspension(t *testing.T) {
	bs, fakeRemote := newHealthTestEngine(t)

	// Initially, eviction should be enabled and remote should be healthy.
	stats := bs.GetCacheStats()
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

	stats = bs.GetCacheStats()
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

	stats = bs.GetCacheStats()
	if !stats.RemoteHealthy {
		t.Fatal("expected remote healthy after recovery")
	}
	if stats.EvictionSuspended {
		t.Fatal("expected eviction re-enabled after recovery")
	}
}

// TestEngineCacheStatsHealthFields verifies CacheStats includes correct
// remote_healthy, eviction_suspended, and outage_duration_seconds values
// in both healthy and unhealthy states.
func TestEngineCacheStatsHealthFields(t *testing.T) {
	bs, fakeRemote := newHealthTestEngine(t)

	// Healthy state.
	stats := bs.GetCacheStats()
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

	stats = bs.GetCacheStats()
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

	stats = bs.GetCacheStats()
	if !stats.RemoteHealthy {
		t.Fatal("expected RemoteHealthy == true after recovery")
	}
	if stats.OutageDurationSecs != 0 {
		t.Fatalf("expected OutageDurationSecs == 0 after recovery, got %f", stats.OutageDurationSecs)
	}
}

// Compile-time check that fakeRemoteStore satisfies remote.RemoteStore.
var _ remote.RemoteStore = (*fakeRemoteStore)(nil)
