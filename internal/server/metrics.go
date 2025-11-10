package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Server Metrics and Monitoring
// ============================================================================
//
// This module provides comprehensive metrics collection for monitoring
// server health, performance, and resource utilization in production.
//
// Metrics Categories:
// 1. Connection Metrics: Active connections, total accepted/rejected
// 2. Request Metrics: RPC call counts, success/error rates
// 3. Performance Metrics: Latency percentiles, throughput
// 4. Error Metrics: Timeout counts, parse errors, handler errors
//
// Design Rationale:
// - Lock-free atomic operations for high-performance counters
// - Minimal overhead (<1% CPU impact in typical workloads)
// - Periodic logging for easy troubleshooting without external tools
// - Foundation for future integration with Prometheus/StatsD

// ServerMetrics tracks server-wide operational statistics.
// All fields use atomic operations for thread-safe lock-free updates.
type ServerMetrics struct {
	// Connection metrics
	connectionsTotal    atomic.Uint64 // Total connections accepted
	connectionsRejected atomic.Uint64 // Connections rejected due to limits
	connectionsCurrent  atomic.Int32  // Current active connections (may differ from server.connCount due to timing)

	// Request metrics
	requestsTotal      atomic.Uint64 // Total RPC requests processed
	requestsSuccessful atomic.Uint64 // Requests completed successfully
	requestsErrored    atomic.Uint64 // Requests that returned errors

	// Protocol distribution
	nfsRequestsTotal   atomic.Uint64 // NFS protocol requests
	mountRequestsTotal atomic.Uint64 // Mount protocol requests

	// Error metrics
	parseErrors    atomic.Uint64 // RPC parsing failures
	timeoutErrors  atomic.Uint64 // Read/write/idle timeouts
	handlerPanics  atomic.Uint64 // Recovered panics in handlers
	fragmentErrors atomic.Uint64 // Invalid fragment headers

	// Performance metrics (simple moving averages)
	requestDurations *durationsTracker // Request latency tracking

	// Metrics lifecycle
	startTime time.Time
	lastReset time.Time
	mu        sync.RWMutex // Protects startTime and lastReset
}

// durationsTracker maintains a circular buffer of recent request durations
// for calculating latency statistics without storing all measurements.
type durationsTracker struct {
	mu         sync.Mutex
	durations  []time.Duration
	index      int
	count      int
	maxSamples int
}

// newDurationsTracker creates a new latency tracker with the specified capacity.
func newDurationsTracker(maxSamples int) *durationsTracker {
	return &durationsTracker{
		durations:  make([]time.Duration, maxSamples),
		maxSamples: maxSamples,
	}
}

// Add records a new duration measurement.
func (dt *durationsTracker) Add(d time.Duration) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.durations[dt.index] = d
	dt.index = (dt.index + 1) % dt.maxSamples
	if dt.count < dt.maxSamples {
		dt.count++
	}
}

// Stats returns average, min, and max durations from recent samples.
func (dt *durationsTracker) Stats() (avg, min, max time.Duration) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	if dt.count == 0 {
		return 0, 0, 0
	}

	var total time.Duration
	min = time.Hour // Start with large value
	max = 0

	for i := 0; i < dt.count; i++ {
		d := dt.durations[i]
		total += d
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}

	avg = total / time.Duration(dt.count)
	return avg, min, max
}

// newServerMetrics creates a new metrics collector.
func newServerMetrics() *ServerMetrics {
	now := time.Now()
	return &ServerMetrics{
		requestDurations: newDurationsTracker(1000), // Track last 1000 requests
		startTime:        now,
		lastReset:        now,
	}
}

// ============================================================================
// Metrics Update Methods
// ============================================================================

// RecordConnection increments the total connections counter.
func (m *ServerMetrics) RecordConnection() {
	m.connectionsTotal.Add(1)
	m.connectionsCurrent.Add(1)
}

// RecordConnectionClosed decrements the active connections counter.
func (m *ServerMetrics) RecordConnectionClosed() {
	m.connectionsCurrent.Add(-1)
}

// RecordConnectionRejected increments the rejected connections counter.
func (m *ServerMetrics) RecordConnectionRejected() {
	m.connectionsRejected.Add(1)
}

// RecordRequest increments the total requests counter and tracks duration.
func (m *ServerMetrics) RecordRequest(duration time.Duration, success bool, isNFS bool) {
	m.requestsTotal.Add(1)

	if success {
		m.requestsSuccessful.Add(1)
	} else {
		m.requestsErrored.Add(1)
	}

	if isNFS {
		m.nfsRequestsTotal.Add(1)
	} else {
		m.mountRequestsTotal.Add(1)
	}

	m.requestDurations.Add(duration)
}

// RecordParseError increments the parse error counter.
func (m *ServerMetrics) RecordParseError() {
	m.parseErrors.Add(1)
}

// RecordTimeout increments the timeout error counter.
func (m *ServerMetrics) RecordTimeout() {
	m.timeoutErrors.Add(1)
}

// RecordPanic increments the handler panic counter.
func (m *ServerMetrics) RecordPanic() {
	m.handlerPanics.Add(1)
}

// RecordFragmentError increments the fragment error counter.
func (m *ServerMetrics) RecordFragmentError() {
	m.fragmentErrors.Add(1)
}

// ============================================================================
// Metrics Reporting
// ============================================================================

// MetricsSnapshot contains a point-in-time view of all metrics.
// This struct is safe to read without locks after creation.
type MetricsSnapshot struct {
	// Timestamps
	StartTime    time.Time
	SnapshotTime time.Time
	Uptime       time.Duration

	// Connection metrics
	ConnectionsTotal    uint64
	ConnectionsRejected uint64
	ConnectionsCurrent  int32

	// Request metrics
	RequestsTotal      uint64
	RequestsSuccessful uint64
	RequestsErrored    uint64
	NFSRequests        uint64
	MountRequests      uint64

	// Error metrics
	ParseErrors    uint64
	TimeoutErrors  uint64
	HandlerPanics  uint64
	FragmentErrors uint64

	// Performance metrics
	AvgRequestDuration time.Duration
	MinRequestDuration time.Duration
	MaxRequestDuration time.Duration
	RequestsPerSecond  float64
}

// Snapshot captures current metrics values atomically.
func (m *ServerMetrics) Snapshot() *MetricsSnapshot {
	m.mu.RLock()
	startTime := m.startTime
	m.mu.RUnlock()

	now := time.Now()
	uptime := now.Sub(startTime)

	avg, min, max := m.requestDurations.Stats()

	total := m.requestsTotal.Load()
	rps := float64(total) / uptime.Seconds()

	return &MetricsSnapshot{
		StartTime:           startTime,
		SnapshotTime:        now,
		Uptime:              uptime,
		ConnectionsTotal:    m.connectionsTotal.Load(),
		ConnectionsRejected: m.connectionsRejected.Load(),
		ConnectionsCurrent:  m.connectionsCurrent.Load(),
		RequestsTotal:       total,
		RequestsSuccessful:  m.requestsSuccessful.Load(),
		RequestsErrored:     m.requestsErrored.Load(),
		NFSRequests:         m.nfsRequestsTotal.Load(),
		MountRequests:       m.mountRequestsTotal.Load(),
		ParseErrors:         m.parseErrors.Load(),
		TimeoutErrors:       m.timeoutErrors.Load(),
		HandlerPanics:       m.handlerPanics.Load(),
		FragmentErrors:      m.fragmentErrors.Load(),
		AvgRequestDuration:  avg,
		MinRequestDuration:  min,
		MaxRequestDuration:  max,
		RequestsPerSecond:   rps,
	}
}

// LogSnapshot logs a formatted metrics snapshot at INFO level.
func (m *ServerMetrics) LogSnapshot() {
	snap := m.Snapshot()

	logger.Info("=== Server Metrics ===")
	logger.Info("Uptime: %v", snap.Uptime.Round(time.Second))
	logger.Info("Connections: %d active, %d total, %d rejected",
		snap.ConnectionsCurrent, snap.ConnectionsTotal, snap.ConnectionsRejected)
	logger.Info("Requests: %d total (%.1f req/s), %d successful, %d errored",
		snap.RequestsTotal, snap.RequestsPerSecond,
		snap.RequestsSuccessful, snap.RequestsErrored)
	logger.Info("Protocol: %d NFS, %d Mount",
		snap.NFSRequests, snap.MountRequests)

	if snap.RequestsTotal > 0 {
		logger.Info("Latency: avg=%v min=%v max=%v",
			snap.AvgRequestDuration.Round(time.Microsecond),
			snap.MinRequestDuration.Round(time.Microsecond),
			snap.MaxRequestDuration.Round(time.Microsecond))
	}

	if snap.ParseErrors > 0 || snap.TimeoutErrors > 0 ||
		snap.HandlerPanics > 0 || snap.FragmentErrors > 0 {
		logger.Info("Errors: %d parse, %d timeout, %d panic, %d fragment",
			snap.ParseErrors, snap.TimeoutErrors,
			snap.HandlerPanics, snap.FragmentErrors)
	}
}

// Reset clears all metrics counters.
// This is primarily useful for testing or manual reset via admin interface.
func (m *ServerMetrics) Reset() {
	m.mu.Lock()
	m.lastReset = time.Now()
	m.mu.Unlock()

	// Reset atomic counters
	m.connectionsTotal.Store(0)
	m.connectionsRejected.Store(0)
	m.connectionsCurrent.Store(0)
	m.requestsTotal.Store(0)
	m.requestsSuccessful.Store(0)
	m.requestsErrored.Store(0)
	m.nfsRequestsTotal.Store(0)
	m.mountRequestsTotal.Store(0)
	m.parseErrors.Store(0)
	m.timeoutErrors.Store(0)
	m.handlerPanics.Store(0)
	m.fragmentErrors.Store(0)

	// Reset duration tracker
	m.requestDurations = newDurationsTracker(1000)
}

// startMetricsLogger starts a background goroutine that periodically logs metrics.
// The goroutine will stop when the context is cancelled.
func (s *NFSServer) startMetricsLogger(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		return // Metrics logging disabled
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.metrics.LogSnapshot()
			}
		}
	}()
}
