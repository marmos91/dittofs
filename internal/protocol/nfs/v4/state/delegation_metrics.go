package state

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ============================================================================
// Prometheus Metrics for NFSv4 Delegation Operations
// ============================================================================

// DelegationMetrics provides Prometheus metrics for delegation lifecycle tracking.
// All methods are nil-safe: calls on a nil *DelegationMetrics are no-ops.
type DelegationMetrics struct {
	// grantedTotal counts the total number of delegations granted, labeled by type.
	grantedTotal *prometheus.CounterVec

	// recalledTotal counts the total number of delegations recalled, labeled by type and reason.
	recalledTotal *prometheus.CounterVec

	// active tracks the number of currently active delegations, labeled by type.
	active *prometheus.GaugeVec

	// dirNotificationsSentTotal counts the total number of directory notifications sent.
	dirNotificationsSentTotal prometheus.Counter
}

// NewDelegationMetrics creates and registers delegation metrics with the given
// Prometheus registerer. If reg is nil, metrics are created but not registered
// (useful for testing).
//
// On re-registration (server restart), existing collectors from the registry
// are reused so that metrics continue to be exported correctly.
func NewDelegationMetrics(reg prometheus.Registerer) *DelegationMetrics {
	m := &DelegationMetrics{
		grantedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "delegations_granted_total",
			Help:      "Total number of delegations granted",
		}, []string{"type"}),
		recalledTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "delegations_recalled_total",
			Help:      "Total number of delegations recalled",
		}, []string{"type", "reason"}),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "delegations_active",
			Help:      "Number of currently active delegations",
		}, []string{"type"}),
		dirNotificationsSentTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "dir_notifications_sent_total",
			Help:      "Total number of directory change notifications sent",
		}),
	}

	if reg != nil {
		m.grantedTotal = registerOrReuse(reg, m.grantedTotal).(*prometheus.CounterVec)
		m.recalledTotal = registerOrReuse(reg, m.recalledTotal).(*prometheus.CounterVec)
		m.active = registerOrReuse(reg, m.active).(*prometheus.GaugeVec)
		m.dirNotificationsSentTotal = registerOrReuse(reg, m.dirNotificationsSentTotal).(prometheus.Counter)
	}

	return m
}

// RecordGrant increments the delegation granted counter for the given type.
func (m *DelegationMetrics) RecordGrant(delegationType string) {
	if m == nil {
		return
	}
	m.grantedTotal.WithLabelValues(delegationType).Inc()
	m.active.WithLabelValues(delegationType).Inc()
}

// RecordRecall increments the delegation recalled counter for the given type and reason.
func (m *DelegationMetrics) RecordRecall(delegationType, reason string) {
	if m == nil {
		return
	}
	m.recalledTotal.WithLabelValues(delegationType, reason).Inc()
}

// RecordReturn decrements the active delegation gauge for the given type.
func (m *DelegationMetrics) RecordReturn(delegationType string) {
	if m == nil {
		return
	}
	m.active.WithLabelValues(delegationType).Dec()
}

// RecordDirNotification increments the directory notifications sent counter.
func (m *DelegationMetrics) RecordDirNotification() {
	if m == nil {
		return
	}
	m.dirNotificationsSentTotal.Inc()
}
