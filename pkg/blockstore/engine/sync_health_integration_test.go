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

// waitFor polls cond until it returns true or the deadline elapses. It is the
// standard alternative to a fixed-duration time.Sleep before asserting on
// asynchronous state — the periodic uploader, health monitor, and callback
// dispatch are all event-driven, so polling avoids flakes on slow CI runners
// while keeping the happy path fast.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out after %v waiting for %s", timeout, what)
	}
}

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
	bc, err := fs.NewWithOptions(tmpDir, 0, 0, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 50,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions() error = %v", err)
	}
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}

	rs := newControllableRemoteStore()
	m := NewSyncer(bc, rs, ms, healthTestConfig())
	// Wire the SyncedHashStore on the syncer so the mirror loop's
	// MarkSynced step actually fires (mirror loop short-circuits to a
	// no-op when the SyncedHashStore is nil).
	m.SetSyncedHashStore(ms)
	t.Cleanup(func() {
		_ = m.Close()
		_ = bc.Close()
	})

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
	waitFor(t, 5*time.Second, func() bool {
		return !env.syncer.IsRemoteHealthy()
	}, "remote to be detected unhealthy after simulated outage")

	// Write a block to the local store while unhealthy.
	payloadID := "export/circuit-breaker-test.bin"
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := env.local.AppendWrite(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("AppendWrite failed: %v", err)
	}

	// Flush to disk (block becomes Local state).
	if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	// Drive rollup so the AppendWrite-staged bytes land in the CAS
	// chunk store and become eligible for the mirror loop.
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 16; i++ {
		if err := env.local.ForceRollupForTest(ctx, payloadID); err != nil {
			t.Fatalf("ForceRollupForTest: %v", err)
		}
		if env.local.IntervalsLenForTest(payloadID) == 0 {
			break
		}
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

	// Wait for recovery detection + periodic upload drain. Poll instead of a
	// fixed sleep so we don't flake on slow CI runners (the deterministic
	// failure mode is recovery + upload taking longer than the budget).
	waitFor(t, 5*time.Second, func() bool {
		return env.syncer.IsRemoteHealthy() && memStore.BlockCount() > 0
	}, "remote to recover and block to be uploaded")

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
	waitFor(t, 5*time.Second, func() bool {
		return !env.syncer.IsRemoteHealthy()
	}, "remote to be detected unhealthy after simulated outage")

	// Write 3 blocks during outage.
	payloadID := "export/drain-test.bin"
	for i := 0; i < 3; i++ {
		data := make([]byte, 1024)
		for j := range data {
			data[j] = byte((i + j) % 256)
		}
		offset := uint64(i) * BlockSize // Each write goes to a different block
		if err := env.local.AppendWrite(ctx, payloadID, data, offset); err != nil {
			t.Fatalf("AppendWrite block %d failed: %v", i, err)
		}
	}

	if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	// Drive rollup synchronously to land bytes in the CAS chunk store.
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 16; i++ {
		if err := env.local.ForceRollupForTest(ctx, payloadID); err != nil {
			t.Fatalf("ForceRollupForTest: %v", err)
		}
		if env.local.IntervalsLenForTest(payloadID) == 0 {
			break
		}
	}
	env.local.SyncFileBlocks(ctx)

	// Verify no uploads during outage.
	memStore := env.remote.RemoteStore.(*remotememory.Store)
	if memStore.BlockCount() != 0 {
		t.Fatalf("expected 0 blocks in remote during outage, got %d", memStore.BlockCount())
	}

	// Restore health.
	env.remote.SetHealthy(true)

	// Wait for recovery + periodic upload drain. Poll instead of a fixed
	// sleep so we don't flake on slow CI runners (Windows + Linux 1.25.x
	// have been observed to need >500ms for the full drain).
	waitFor(t, 5*time.Second, func() bool {
		return env.syncer.IsRemoteHealthy() && memStore.BlockCount() == 3
	}, "remote to recover and 3 blocks to drain")

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

	// Simulate outage and wait for the unhealthy callback.
	env.remote.SetHealthy(false)
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= 1 && events[0] == false
	}, "unhealthy callback to fire")

	// Restore health and wait for the recovery callback.
	env.remote.SetHealthy(true)
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= 2 && events[1] == true
	}, "recovery callback to fire")

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
