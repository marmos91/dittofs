package engine

import (
	"context"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// healthTransitionCallback is invoked when the health state changes.
// The healthy parameter is true when transitioning to healthy, false when transitioning to unhealthy.
type healthTransitionCallback func(healthy bool)

// HealthMonitor periodically probes a remote store and manages a healthy/unhealthy
// state machine. It is the single source of truth for remote store availability.
//
// State transitions
//   - Starts healthy.
//   - After failureThreshold consecutive probe failures: transitions to unhealthy.
//   - After 1 successful probe while unhealthy: transitions back to healthy.
//
// When probeFunc is nil (local-only shares), the monitor always reports healthy
// and never starts a background goroutine.
type HealthMonitor struct {
	probeFunc         func(ctx context.Context) error
	healthyInterval   time.Duration
	unhealthyInterval time.Duration
	failureThreshold  int32

	healthy             atomic.Bool
	consecutiveFailures atomic.Int32
	unhealthySince      atomic.Int64 // Unix nanos; 0 when healthy

	onTransition healthTransitionCallback
	mu           gosync.Mutex // Protects onTransition

	stopCh   chan struct{}
	stopOnce gosync.Once
	// wg counts the monitor loop so Stop can join it, keeping a probe from
	// running against a remote store the caller is about to close.
	wg gosync.WaitGroup
}

// NewHealthMonitor creates a new HealthMonitor. If probeFunc is nil, the monitor
// always reports healthy and Start() is a no-op.
func NewHealthMonitor(probeFunc func(ctx context.Context) error, config RemoteSyncConfig) *HealthMonitor {
	interval := config.HealthCheckInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	unhealthyInterval := config.UnhealthyCheckInterval
	if unhealthyInterval <= 0 {
		unhealthyInterval = 5 * time.Second
	}
	threshold := int32(config.HealthCheckFailureThreshold)
	if threshold <= 0 {
		threshold = 3
	}

	hm := &HealthMonitor{
		probeFunc:         probeFunc,
		healthyInterval:   interval,
		unhealthyInterval: unhealthyInterval,
		failureThreshold:  threshold,
		stopCh:            make(chan struct{}),
	}
	hm.healthy.Store(true)

	return hm
}

// Start launches the health monitor goroutine. If probeFunc is nil, this is a no-op.
//
// An eager initial probe runs synchronously before launching the background loop.
// If the probe fails, the monitor starts in unhealthy state so the periodic
// uploader doesn't waste cycles attempting uploads against a broken remote.
func (hm *HealthMonitor) Start(ctx context.Context) {
	if hm.probeFunc == nil {
		return
	}

	// Eager probe: verify connectivity before assuming healthy.
	if err := hm.probeFunc(ctx); err != nil {
		hm.healthy.Store(false)
		hm.unhealthySince.Store(time.Now().UnixNano())
		hm.consecutiveFailures.Store(1)
		logger.Warn("Remote store initial probe failed, starting as unhealthy",
			"error", err)
		hm.fireCallback(false)
	} else {
		logger.Info("Remote store initial probe succeeded")
	}

	hm.wg.Add(1)
	go func() {
		defer hm.wg.Done()
		hm.monitorLoop(ctx)
	}()
}

// Stop signals the health monitor goroutine to exit and waits for it. Safe to
// call multiple times. The wait is bounded because the loop can be inside a
// probe against an unreachable remote; a monitor that outlives its budget is
// logged rather than allowed to wedge shutdown.
func (hm *HealthMonitor) Stop() {
	hm.stopOnce.Do(func() {
		close(hm.stopCh)
	})
	if !waitBounded(&hm.wg, defaultShutdownTimeout) {
		logger.Warn("Health monitor did not exit before shutdown timeout")
	}
}

// IsHealthy returns the current health state. Always true if probeFunc is nil.
func (hm *HealthMonitor) IsHealthy() bool {
	return hm.healthy.Load()
}

// SetTransitionCallback sets the callback invoked on health state changes.
func (hm *HealthMonitor) SetTransitionCallback(fn healthTransitionCallback) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	hm.onTransition = fn
}

// OutageDuration returns how long the remote has been unhealthy.
// Returns 0 when healthy.
func (hm *HealthMonitor) OutageDuration() time.Duration {
	since := hm.unhealthySince.Load()
	if since == 0 {
		return 0
	}
	return time.Since(time.Unix(0, since))
}

// monitorLoop runs the probe on a ticker, adjusting interval based on health state.
func (hm *HealthMonitor) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(hm.healthyInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// A tick queued while the previous probe was running is still
			// pending when a stop arrives, and select picks randomly between
			// the two. Re-check the stop first so Stop's join never waits on
			// one more probe round-trip against an unreachable remote.
			select {
			case <-hm.stopCh:
				return
			default:
			}
			err := hm.probeFunc(ctx)
			if err != nil {
				// Don't count context cancellation as a health failure — we're shutting down.
				if ctx.Err() != nil {
					return
				}
				newCount := hm.consecutiveFailures.Add(1)
				logger.Debug("Health probe failed", "error", err, "consecutive_failures", newCount)

				if newCount >= hm.failureThreshold && hm.healthy.Load() {
					hm.healthy.Store(false)
					hm.unhealthySince.Store(time.Now().UnixNano())
					logger.Warn("Remote store marked unhealthy", "consecutive_failures", newCount)
					hm.fireCallback(false)
					// Switch to faster probing for quicker recovery detection
					ticker.Reset(hm.unhealthyInterval)
				}
			} else {
				hm.consecutiveFailures.Store(0)

				if !hm.healthy.Load() {
					duration := hm.OutageDuration()
					hm.healthy.Store(true)
					hm.unhealthySince.Store(0)
					logger.Info("Remote store recovered", "outage_duration", duration)
					hm.fireCallback(true)
					// Switch back to normal probing interval
					ticker.Reset(hm.healthyInterval)
				}
			}

		case <-hm.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// fireCallback invokes the transition callback if set.
func (hm *HealthMonitor) fireCallback(healthy bool) {
	hm.mu.Lock()
	fn := hm.onTransition
	hm.mu.Unlock()

	if fn != nil {
		fn(healthy)
	}
}

// CanEvict reports whether reclaiming local bytes is safe: they may only be
// dropped when something can fetch them back. That is carveActive — the carve
// path wired to a remote — AND that remote being healthy.
//
// carveActive is deliberately the same flag that decides whether a record can
// ever become synced, so "may be evicted" and "can be re-fetched" are one answer
// by construction rather than two that agree by luck. Health alone is not
// enough: IsRemoteHealthy reports true for a nil monitor, so a share with no
// remote at all reads as healthy, and a journal carrying synced records from a
// previous remote-backed life would then satisfy the eviction gate and lose the
// only copy of those bytes.
func (m *RemoteSync) CanEvict() bool {
	return m.carveActive.Load() && m.IsRemoteHealthy()
}

// IsRemoteHealthy returns the health state of the remote store.
// Returns true when there is no HealthMonitor (local-only mode) — which is why
// it is not sufficient on its own to decide whether eviction is safe. Use
// CanEvict for that.
//
// decision: healthMonitor is read without m.mu here, in RemoteOutageDuration
// and in Close, although newHealthMonitorLocked writes it under that lock.
// Unlike the carve wiring it has no setter: startLocked is its only writer,
// writes it once and never clears or replaces it, and every reader either is a
// goroutine startLocked spawned after that write (the carve dispatcher) or
// reaches the syncer through a share that does not serve until Start has
// returned. Locking would put an m.mu acquisition on the fetch and readahead
// read paths, which are lock-free on purpose. Withdraw the exemption the
// moment anything re-wires or clears healthMonitor after Start, or calls Start
// twice on one syncer: the field then needs m.mu or an atomic cell, and the
// read path pays for it.
func (m *RemoteSync) IsRemoteHealthy() bool {
	if m.healthMonitor == nil {
		return true
	}
	return m.healthMonitor.IsHealthy()
}

// RemoteOutageDuration returns how long the remote store has been unhealthy.
// Returns 0 when healthy or when there is no HealthMonitor.
func (m *RemoteSync) RemoteOutageDuration() time.Duration {
	if m.healthMonitor == nil {
		return 0
	}
	return m.healthMonitor.OutageDuration()
}

// remoteUnavailableError returns an ErrRemoteUnavailable wrapped with outage duration context.
func (m *RemoteSync) remoteUnavailableError() error {
	dur := m.RemoteOutageDuration()
	return fmt.Errorf("remote store unavailable (offline for %s): %w", dur.Truncate(time.Second), block.ErrRemoteUnavailable)
}

// OfflineReadsBlocked returns the count of read operations that failed
// because the requested blocks were remote-only during an outage.
func (m *RemoteSync) OfflineReadsBlocked() int64 {
	return m.offlineReadsBlocked.Load()
}

// logOfflineRead logs a read failure due to remote unavailability.
// First failure after a healthy->unhealthy transition logs at WARN level
// subsequent failures log at DEBUG to avoid log spam.
func (m *RemoteSync) logOfflineRead(method, payloadID string, blockIdx uint64) {
	if m.firstOfflineRead.CompareAndSwap(false, true) {
		logger.Warn("Read blocked: remote store unavailable",
			"method", method,
			"payloadID", payloadID,
			"blockIdx", blockIdx,
			"outage_duration", m.RemoteOutageDuration().Truncate(time.Second))
	} else {
		logger.Debug("Read blocked: remote store unavailable",
			"method", method,
			"payloadID", payloadID,
			"blockIdx", blockIdx)
	}
}

// newHealthMonitorLocked creates and wires the health monitor for the remote
// store, without starting it. Must be called with m.mu held.
func (m *RemoteSync) newHealthMonitorLocked() *HealthMonitor {
	m.healthMonitor = NewHealthMonitor(m.remoteStore.HealthCheck, m.config)
	// Wrap the user's callback to also reset the offline-read WARN flag
	// on each healthy->unhealthy transition.
	userCallback := m.onHealthChanged
	m.healthMonitor.SetTransitionCallback(func(healthy bool) {
		if !healthy {
			m.firstOfflineRead.Store(false)
		}
		if userCallback != nil {
			userCallback(healthy)
		}
	})
	return m.healthMonitor
}

// HealthCheck verifies the remote store is accessible.
// Returns nil (healthy) when remoteStore is nil -- local-only mode is valid.
func (m *RemoteSync) HealthCheck(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return ErrClosed
	}

	if m.remoteStore == nil {
		return nil // Local-only mode is healthy
	}

	return m.remoteStore.HealthCheck(ctx)
}
