package sync

import (
	"context"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// HealthTransitionCallback is invoked when the health state changes.
// The healthy parameter is true when transitioning to healthy, false when transitioning to unhealthy.
type HealthTransitionCallback func(healthy bool)

// HealthMonitor periodically probes a remote store and manages a healthy/unhealthy
// state machine. It is the single source of truth for remote store availability.
//
// State transitions:
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

	onTransition HealthTransitionCallback
	mu           gosync.Mutex // Protects onTransition

	stopCh   chan struct{}
	stopOnce gosync.Once
}

// NewHealthMonitor creates a new HealthMonitor. If probeFunc is nil, the monitor
// always reports healthy and Start() is a no-op.
func NewHealthMonitor(probeFunc func(ctx context.Context) error, config Config) *HealthMonitor {
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

	go hm.monitorLoop(ctx)
}

// Stop signals the health monitor goroutine to exit. Safe to call multiple times.
func (hm *HealthMonitor) Stop() {
	hm.stopOnce.Do(func() {
		close(hm.stopCh)
	})
}

// IsHealthy returns the current health state. Always true if probeFunc is nil.
func (hm *HealthMonitor) IsHealthy() bool {
	return hm.healthy.Load()
}

// SetTransitionCallback sets the callback invoked on health state changes.
func (hm *HealthMonitor) SetTransitionCallback(fn HealthTransitionCallback) {
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
