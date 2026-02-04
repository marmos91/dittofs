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
	LabelShare     = "share"
	LabelType      = "type"
	LabelStatus    = "status"
	LabelReason    = "reason"
	LabelAdapter   = "adapter"
	LabelEvent     = "event"
	LabelLimitType = "limit_type"
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
}
