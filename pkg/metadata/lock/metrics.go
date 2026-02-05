package lock

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ============================================================================
// Prometheus Metrics for Locks and Connections
// ============================================================================

// Label constants for metrics.
const (
	LabelShare      = "share"
	LabelType       = "type"
	LabelStatus     = "status"
	LabelReason     = "reason"
	LabelAdapter    = "adapter"
	LabelEvent      = "event"
	LabelLimitType  = "limit_type"
	LabelInitiator  = "initiator"
	LabelConflicting = "conflicting"
	LabelResolution = "resolution"
	LabelTrigger    = "trigger"
	LabelTarget     = "target"
)

// Status constants for lock operations.
const (
	StatusGranted  = "granted"
	StatusDenied   = "denied"
	StatusDeadlock = "deadlock"
)

// Reason constants for lock release.
const (
	ReasonExplicit     = "explicit"
	ReasonTimeout      = "timeout"
	ReasonDisconnect   = "disconnect"
	ReasonGraceExpired = "grace_expired"
)

// Cross-protocol initiator constants.
const (
	InitiatorNFS = "nfs"
	InitiatorSMB = "smb"
)

// Cross-protocol conflicting type constants.
const (
	ConflictingNFSLock   = "nfs_lock"
	ConflictingSMBLease  = "smb_lease"
)

// Cross-protocol resolution constants.
const (
	ResolutionDenied         = "denied"
	ResolutionBreakInitiated = "break_initiated"
	ResolutionBreakCompleted = "break_completed"
)

// Cross-protocol trigger constants.
const (
	TriggerNFSWrite  = "nfs_write"
	TriggerNFSLock   = "nfs_lock"
	TriggerNFSRemove = "nfs_remove"
)

// Cross-protocol target constants.
const (
	TargetSMBWriteLease  = "smb_write_lease"
	TargetSMBHandleLease = "smb_handle_lease"
)

// Metrics provides Prometheus metrics for lock and connection tracking.
type Metrics struct {
	// Lock operation counters
	lockAcquireTotal *prometheus.CounterVec
	lockReleaseTotal *prometheus.CounterVec

	// Lock state gauges
	lockActiveGauge  *prometheus.GaugeVec
	lockBlockedGauge *prometheus.GaugeVec

	// Lock timing histograms
	lockBlockingDuration *prometheus.HistogramVec
	lockHoldDuration     *prometheus.HistogramVec

	// Connection metrics
	connectionActiveGauge *prometheus.GaugeVec
	connectionTotal       *prometheus.CounterVec

	// Grace period metrics
	gracePeriodActive    prometheus.Gauge
	gracePeriodRemaining prometheus.Gauge
	reclaimTotal         *prometheus.CounterVec

	// Limit metrics
	lockLimitHits *prometheus.CounterVec

	// Deadlock detection
	deadlockDetected prometheus.Counter

	// Cross-protocol metrics
	crossProtocolConflictTotal   *prometheus.CounterVec
	crossProtocolBreakDuration   *prometheus.HistogramVec

	// Flag to track if metrics are registered
	registered bool
}

// NewMetrics creates and registers lock metrics.
// If registry is nil, metrics will be created but not registered (useful for testing).
func NewMetrics(registry prometheus.Registerer) *Metrics {
	m := &Metrics{
		lockAcquireTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "acquire_total",
				Help:      "Total number of lock acquire attempts",
			},
			[]string{LabelShare, LabelType, LabelStatus},
		),

		lockReleaseTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "release_total",
				Help:      "Total number of lock releases",
			},
			[]string{LabelShare, LabelReason},
		),

		lockActiveGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "active",
				Help:      "Number of currently active locks",
			},
			[]string{LabelShare, LabelType},
		),

		lockBlockedGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "blocked",
				Help:      "Number of blocked lock requests waiting",
			},
			[]string{LabelShare},
		),

		lockBlockingDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "blocking_duration_seconds",
				Help:      "Time spent waiting for a lock",
				Buckets:   []float64{0.001, 0.01, 0.1, 0.5, 1, 5, 10, 30, 60},
			},
			[]string{LabelShare},
		),

		lockHoldDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "hold_duration_seconds",
				Help:      "Time a lock was held before release",
				Buckets:   []float64{0.1, 1, 5, 10, 30, 60, 300, 600, 1800, 3600},
			},
			[]string{LabelShare, LabelType},
		),

		connectionActiveGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "dittofs",
				Subsystem: "connections",
				Name:      "active",
				Help:      "Number of active client connections",
			},
			[]string{LabelAdapter},
		),

		connectionTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "dittofs",
				Subsystem: "connections",
				Name:      "total",
				Help:      "Total number of connection events",
			},
			[]string{LabelAdapter, LabelEvent},
		),

		gracePeriodActive: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "grace_period_active",
				Help:      "1 if grace period is active, 0 otherwise",
			},
		),

		gracePeriodRemaining: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "grace_period_remaining_seconds",
				Help:      "Seconds remaining in grace period (0 if inactive)",
			},
		),

		reclaimTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "reclaim_total",
				Help:      "Total number of lock reclaim attempts",
			},
			[]string{LabelStatus},
		),

		lockLimitHits: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "limit_hits_total",
				Help:      "Number of times lock limits were hit",
			},
			[]string{LabelLimitType},
		),

		deadlockDetected: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "deadlock_detected_total",
				Help:      "Number of deadlocks detected",
			},
		),

		crossProtocolConflictTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "cross_protocol_conflict_total",
				Help:      "Total number of cross-protocol lock conflicts",
			},
			[]string{LabelInitiator, LabelConflicting, LabelResolution},
		),

		crossProtocolBreakDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "dittofs",
				Subsystem: "locks",
				Name:      "cross_protocol_break_duration_seconds",
				Help:      "Time taken to complete a cross-protocol lease break",
				// Buckets from 0.1s to ~100s exponential
				// SMB lease break timeout is 35s, so we need coverage up to there
				Buckets:   []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 35, 50, 100},
			},
			[]string{LabelTrigger, LabelTarget},
		),
	}

	// Register with registry if provided
	if registry != nil {
		registry.MustRegister(
			m.lockAcquireTotal,
			m.lockReleaseTotal,
			m.lockActiveGauge,
			m.lockBlockedGauge,
			m.lockBlockingDuration,
			m.lockHoldDuration,
			m.connectionActiveGauge,
			m.connectionTotal,
			m.gracePeriodActive,
			m.gracePeriodRemaining,
			m.reclaimTotal,
			m.lockLimitHits,
			m.deadlockDetected,
			m.crossProtocolConflictTotal,
			m.crossProtocolBreakDuration,
		)
		m.registered = true
	}

	return m
}

// ============================================================================
// Lock Operation Metrics
// ============================================================================

// ObserveLockAcquire records a lock acquire attempt.
func (m *Metrics) ObserveLockAcquire(share string, lockType LockType, success bool) {
	if m == nil {
		return
	}

	typeLabel := "shared"
	if lockType == LockTypeExclusive {
		typeLabel = "exclusive"
	}

	status := StatusGranted
	if !success {
		status = StatusDenied
	}

	m.lockAcquireTotal.WithLabelValues(share, typeLabel, status).Inc()
}

// ObserveLockRelease records a lock release.
func (m *Metrics) ObserveLockRelease(share string, reason string) {
	if m == nil {
		return
	}
	m.lockReleaseTotal.WithLabelValues(share, reason).Inc()
}

// SetActiveLocks sets the number of active locks.
func (m *Metrics) SetActiveLocks(share string, lockType LockType, count float64) {
	if m == nil {
		return
	}

	typeLabel := "shared"
	if lockType == LockTypeExclusive {
		typeLabel = "exclusive"
	}

	m.lockActiveGauge.WithLabelValues(share, typeLabel).Set(count)
}

// SetBlockedLocks sets the number of blocked lock requests.
func (m *Metrics) SetBlockedLocks(share string, count float64) {
	if m == nil {
		return
	}
	m.lockBlockedGauge.WithLabelValues(share).Set(count)
}

// ObserveBlockingDuration records time spent waiting for a lock.
func (m *Metrics) ObserveBlockingDuration(share string, duration time.Duration) {
	if m == nil {
		return
	}
	m.lockBlockingDuration.WithLabelValues(share).Observe(duration.Seconds())
}

// ObserveLockHoldDuration records time a lock was held.
func (m *Metrics) ObserveLockHoldDuration(share string, lockType LockType, duration time.Duration) {
	if m == nil {
		return
	}

	typeLabel := "shared"
	if lockType == LockTypeExclusive {
		typeLabel = "exclusive"
	}

	m.lockHoldDuration.WithLabelValues(share, typeLabel).Observe(duration.Seconds())
}

// ============================================================================
// Connection Metrics
// ============================================================================

// SetActiveConnections sets the number of active connections for an adapter.
func (m *Metrics) SetActiveConnections(adapter string, count float64) {
	if m == nil {
		return
	}
	m.connectionActiveGauge.WithLabelValues(adapter).Set(count)
}

// ObserveConnection records a connection event.
func (m *Metrics) ObserveConnection(adapter string, event string) {
	if m == nil {
		return
	}
	m.connectionTotal.WithLabelValues(adapter, event).Inc()
}

// ============================================================================
// Grace Period Metrics
// ============================================================================

// SetGracePeriodActive sets whether grace period is active.
func (m *Metrics) SetGracePeriodActive(active bool) {
	if m == nil {
		return
	}
	val := 0.0
	if active {
		val = 1.0
	}
	m.gracePeriodActive.Set(val)
}

// SetGracePeriodRemaining sets the remaining time in grace period.
func (m *Metrics) SetGracePeriodRemaining(seconds float64) {
	if m == nil {
		return
	}
	m.gracePeriodRemaining.Set(seconds)
}

// ObserveReclaim records a lock reclaim attempt.
func (m *Metrics) ObserveReclaim(success bool) {
	if m == nil {
		return
	}
	status := StatusGranted
	if !success {
		status = StatusDenied
	}
	m.reclaimTotal.WithLabelValues(status).Inc()
}

// ============================================================================
// Limit Metrics
// ============================================================================

// ObserveLockLimitHit records when a lock limit is hit.
func (m *Metrics) ObserveLockLimitHit(limitType string) {
	if m == nil {
		return
	}
	m.lockLimitHits.WithLabelValues(limitType).Inc()
}

// ============================================================================
// Deadlock Metrics
// ============================================================================

// ObserveDeadlock records a detected deadlock.
func (m *Metrics) ObserveDeadlock() {
	if m == nil {
		return
	}
	m.deadlockDetected.Inc()
}

// ============================================================================
// Cross-Protocol Metrics
// ============================================================================

// RecordCrossProtocolConflict records a cross-protocol lock conflict.
//
// Parameters:
//   - initiator: The protocol that initiated the operation (nfs/smb)
//   - conflicting: The type of conflicting lock (nfs_lock/smb_lease)
//   - resolution: How the conflict was resolved (denied/break_initiated/break_completed)
//
// Example usage:
//   - NFS WRITE conflicts with SMB Write lease, break initiated:
//     RecordCrossProtocolConflict(InitiatorNFS, ConflictingSMBLease, ResolutionBreakInitiated)
//   - SMB lock request denied due to NFS lock:
//     RecordCrossProtocolConflict(InitiatorSMB, ConflictingNFSLock, ResolutionDenied)
func (m *Metrics) RecordCrossProtocolConflict(initiator, conflicting, resolution string) {
	if m == nil {
		return
	}
	m.crossProtocolConflictTotal.WithLabelValues(initiator, conflicting, resolution).Inc()
}

// RecordCrossProtocolBreakDuration records the time taken to complete a cross-protocol
// lease break operation.
//
// Parameters:
//   - trigger: What triggered the break (nfs_write/nfs_lock/nfs_remove)
//   - target: The type of lease being broken (smb_write_lease/smb_handle_lease)
//   - duration: Time from break initiation to completion
//
// Example usage:
//   - NFS WRITE triggered SMB Write lease break that took 2.5s:
//     RecordCrossProtocolBreakDuration(TriggerNFSWrite, TargetSMBWriteLease, 2.5*time.Second)
func (m *Metrics) RecordCrossProtocolBreakDuration(trigger, target string, duration time.Duration) {
	if m == nil {
		return
	}
	m.crossProtocolBreakDuration.WithLabelValues(trigger, target).Observe(duration.Seconds())
}

// ============================================================================
// Collector Interface (optional)
// ============================================================================

// Describe implements prometheus.Collector.
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	if m == nil || !m.registered {
		return
	}

	m.lockAcquireTotal.Describe(ch)
	m.lockReleaseTotal.Describe(ch)
	m.lockActiveGauge.Describe(ch)
	m.lockBlockedGauge.Describe(ch)
	m.lockBlockingDuration.Describe(ch)
	m.lockHoldDuration.Describe(ch)
	m.connectionActiveGauge.Describe(ch)
	m.connectionTotal.Describe(ch)
	ch <- m.gracePeriodActive.Desc()
	ch <- m.gracePeriodRemaining.Desc()
	m.reclaimTotal.Describe(ch)
	m.lockLimitHits.Describe(ch)
	ch <- m.deadlockDetected.Desc()
	m.crossProtocolConflictTotal.Describe(ch)
	m.crossProtocolBreakDuration.Describe(ch)
}

// Collect implements prometheus.Collector.
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	if m == nil || !m.registered {
		return
	}

	m.lockAcquireTotal.Collect(ch)
	m.lockReleaseTotal.Collect(ch)
	m.lockActiveGauge.Collect(ch)
	m.lockBlockedGauge.Collect(ch)
	m.lockBlockingDuration.Collect(ch)
	m.lockHoldDuration.Collect(ch)
	m.connectionActiveGauge.Collect(ch)
	m.connectionTotal.Collect(ch)
	ch <- m.gracePeriodActive
	ch <- m.gracePeriodRemaining
	m.reclaimTotal.Collect(ch)
	m.lockLimitHits.Collect(ch)
	ch <- m.deadlockDetected
	m.crossProtocolConflictTotal.Collect(ch)
	m.crossProtocolBreakDuration.Collect(ch)
}

// ============================================================================
// Package-Level Metric Functions
// ============================================================================
//
// These functions provide convenient access to cross-protocol metrics without
// requiring access to a Metrics instance. They use a package-level instance
// that is safe to call even before initialization (they are no-ops in that case).

var globalMetrics *Metrics

// SetGlobalMetrics sets the global metrics instance used by package-level functions.
// This should be called during initialization with a registered Metrics instance.
func SetGlobalMetrics(m *Metrics) {
	globalMetrics = m
}

// RecordCrossProtocolConflict is a package-level function for recording cross-protocol conflicts.
//
// This is a convenience wrapper around Metrics.RecordCrossProtocolConflict that uses
// the global metrics instance. Safe to call before metrics initialization (no-op).
//
// Parameters:
//   - initiator: The protocol that initiated the operation (InitiatorNFS/InitiatorSMB)
//   - conflicting: The type of conflicting lock (ConflictingNFSLock/ConflictingSMBLease)
//   - resolution: How the conflict was resolved (ResolutionDenied/ResolutionBreakInitiated)
func RecordCrossProtocolConflict(initiator, conflicting, resolution string) {
	if globalMetrics != nil {
		globalMetrics.RecordCrossProtocolConflict(initiator, conflicting, resolution)
	}
}

// RecordCrossProtocolBreakDuration is a package-level function for recording break durations.
//
// This is a convenience wrapper around Metrics.RecordCrossProtocolBreakDuration that uses
// the global metrics instance. Safe to call before metrics initialization (no-op).
//
// Parameters:
//   - trigger: What triggered the break (TriggerNFSWrite/TriggerNFSLock/TriggerNFSRemove)
//   - target: The type of lease being broken (TargetSMBWriteLease/TargetSMBHandleLease)
//   - duration: Time from break initiation to completion
func RecordCrossProtocolBreakDuration(trigger, target string, duration time.Duration) {
	if globalMetrics != nil {
		globalMetrics.RecordCrossProtocolBreakDuration(trigger, target, duration)
	}
}
