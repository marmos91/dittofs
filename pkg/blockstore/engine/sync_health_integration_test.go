package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// controllableRemoteStore wraps a memory remote store with a controllable
// health check. When healthy is false, both HealthCheck and Healthcheck
// simulate an outage. All other methods delegate to the wrapped store.
//
// Both probes are overridden because RemoteStore now requires the
// lowercase-c Healthcheck (returning health.Report) alongside the
// legacy capital-C HealthCheck. Without overriding both, Go's interface
// embedding would silently dispatch Healthcheck to the underlying
// memory store, ignoring this fake's `healthy` flag.
type controllableRemoteStore struct {
	remote.RemoteStore
	healthy atomic.Bool
}

func newControllableRemoteStore() *controllableRemoteStore {
	s := &controllableRemoteStore{RemoteStore: remotememory.New()}
	s.healthy.Store(true)
	return s
}

func (c *controllableRemoteStore) HealthCheck(ctx context.Context) error {
	if !c.healthy.Load() {
		return errors.New("simulated S3 outage")
	}
	return c.RemoteStore.HealthCheck(ctx)
}

func (c *controllableRemoteStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if !c.healthy.Load() {
		return health.NewUnhealthyReport("simulated S3 outage", time.Since(start))
	}
	return c.RemoteStore.Healthcheck(ctx)
}

func (c *controllableRemoteStore) SetHealthy(h bool) { c.healthy.Store(h) }

// healthTestConfig returns a syncer Config with short intervals for testing.
func healthTestConfig() SyncerConfig {
	return SyncerConfig{
		ParallelUploads:             4,
		ParallelDownloads:           4,
		PrefetchBlocks:              0,
		UploadInterval:              50 * time.Millisecond,
		UploadDelay:                 0,
		HealthCheckInterval:         20 * time.Millisecond,
		HealthCheckFailureThreshold: 2,
		UnhealthyCheckInterval:      10 * time.Millisecond,
	}
}

// healthTestEnv holds test environment components for health integration tests.
type healthTestEnv struct {
	syncer *Syncer
	remote *controllableRemoteStore
	local  *fs.FSStore
}

// newHealthTestEnv creates a syncer with a controllable remote store and short
// health/upload intervals for testing. Cleanup is registered via t.Cleanup.
func newHealthTestEnv(t *testing.T) *healthTestEnv {
	t.Helper()
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmpDir, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New() error = %v", err)
	}

	rs := newControllableRemoteStore()
	m := NewSyncer(bc, rs, ms, healthTestConfig())
	t.Cleanup(func() { _ = m.Close() })

	return &healthTestEnv{
		syncer: m,
		remote: rs,
		local:  bc,
	}
}

// TestHealthMonitorCircuitBreaker verifies that uploads pause during a remote
// store outage and resume automatically when health recovers.
func TestHealthMonitorCircuitBreaker(t *testing.T) {
	env := newHealthTestEnv(t)

	ctx := context.Background()
	env.local.Start(ctx)
	env.syncer.Start(ctx)

	// Simulate outage BEFORE writing data, so the circuit breaker is tripped
	// before the periodic uploader can sync the block.
	env.remote.SetHealthy(false)

	// Wait for health monitor to detect failure (threshold=2 failures at 20ms interval).
	time.Sleep(150 * time.Millisecond)

	if env.syncer.IsRemoteHealthy() {
		t.Fatal("expected remote to be unhealthy after simulated outage")
	}

	// Write a block to the local store while unhealthy.
	payloadID := "export/circuit-breaker-test.bin"
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := env.local.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Flush to disk (block becomes Local state).
	if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	env.local.SyncFileBlocks(ctx)

	// Wait for periodic uploader to run -- block should NOT be uploaded.
	time.Sleep(200 * time.Millisecond)

	// The remote store should have no blocks (upload was skipped).
	memStore := env.remote.RemoteStore.(*remotememory.Store)
	if memStore.BlockCount() != 0 {
		t.Fatalf("expected 0 blocks in remote during outage, got %d", memStore.BlockCount())
	}

	// Restore health.
	env.remote.SetHealthy(true)

	// Wait for recovery detection + periodic upload.
	time.Sleep(300 * time.Millisecond)

	if !env.syncer.IsRemoteHealthy() {
		t.Fatal("expected remote to be healthy after recovery")
	}

	// Block should now be uploaded to remote.
	if memStore.BlockCount() == 0 {
		t.Fatal("expected block to be uploaded to remote after recovery")
	}
}

// TestHealthMonitorRecoveryDrain verifies that blocks accumulated during an
// outage are uploaded after recovery.
func TestHealthMonitorRecoveryDrain(t *testing.T) {
	env := newHealthTestEnv(t)

	ctx := context.Background()
	env.local.Start(ctx)
	env.syncer.Start(ctx)

	// Immediately simulate outage.
	env.remote.SetHealthy(false)
	time.Sleep(100 * time.Millisecond) // Wait for health monitor to detect.

	if env.syncer.IsRemoteHealthy() {
		t.Fatal("expected remote to be unhealthy")
	}

	// Write 3 blocks during outage.
	payloadID := "export/drain-test.bin"
	for i := 0; i < 3; i++ {
		data := make([]byte, 1024)
		for j := range data {
			data[j] = byte((i + j) % 256)
		}
		offset := uint64(i) * BlockSize // Each write goes to a different block
		if err := env.local.WriteAt(ctx, payloadID, data, offset); err != nil {
			t.Fatalf("WriteAt block %d failed: %v", i, err)
		}
	}

	if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	env.local.SyncFileBlocks(ctx)

	// Verify no uploads during outage.
	memStore := env.remote.RemoteStore.(*remotememory.Store)
	if memStore.BlockCount() != 0 {
		t.Fatalf("expected 0 blocks in remote during outage, got %d", memStore.BlockCount())
	}

	// Restore health.
	env.remote.SetHealthy(true)

	// Wait for recovery + periodic upload drain.
	time.Sleep(500 * time.Millisecond)

	if !env.syncer.IsRemoteHealthy() {
		t.Fatal("expected remote to be healthy after recovery")
	}

	// All 3 blocks should now be in remote.
	if memStore.BlockCount() != 3 {
		t.Fatalf("expected 3 blocks in remote after drain, got %d", memStore.BlockCount())
	}
}

// TestHealthCallbackInvocation verifies that SetHealthCallback is actually called
// with false on outage and true on recovery.
func TestHealthCallbackInvocation(t *testing.T) {
	env := newHealthTestEnv(t)

	var mu sync.Mutex
	var events []bool

	env.syncer.SetHealthCallback(func(healthy bool) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, healthy)
	})

	ctx := context.Background()
	env.local.Start(ctx)
	env.syncer.Start(ctx)

	// Simulate outage.
	env.remote.SetHealthy(false)
	time.Sleep(150 * time.Millisecond) // Wait for health monitor to detect.

	// Restore health.
	env.remote.SetHealthy(true)
	time.Sleep(100 * time.Millisecond) // Wait for recovery detection.

	mu.Lock()
	defer mu.Unlock()

	if len(events) < 2 {
		t.Fatalf("expected at least 2 callback events [false, true], got %d: %v", len(events), events)
	}
	if events[0] != false {
		t.Fatalf("expected first callback event to be false (unhealthy), got %v", events[0])
	}
	if events[1] != true {
		t.Fatalf("expected second callback event to be true (healthy), got %v", events[1])
	}
}

// TestHealthMonitorNilRemoteStore verifies that a syncer with nil remote store
// always reports IsRemoteHealthy() == true and RemoteOutageDuration() == 0.
func TestHealthMonitorNilRemoteStore(t *testing.T) {
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmpDir, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New() error = %v", err)
	}

	m := NewSyncer(bc, nil, ms, healthTestConfig())
	defer func() { _ = m.Close() }()

	ctx := context.Background()
	m.Start(ctx)

	// Give it a moment for any goroutines to start.
	time.Sleep(50 * time.Millisecond)

	if !m.IsRemoteHealthy() {
		t.Fatal("expected IsRemoteHealthy() == true for nil remote store")
	}
	if d := m.RemoteOutageDuration(); d != 0 {
		t.Fatalf("expected RemoteOutageDuration() == 0 for nil remote store, got %v", d)
	}
}
