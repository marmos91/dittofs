package state

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ============================================================================
// Prometheus Metrics for NFSv4.1 Connection Binding
// ============================================================================

// ConnectionMetrics provides Prometheus metrics for connection binding tracking.
// All methods are nil-safe: calls on a nil *ConnectionMetrics are no-ops.
type ConnectionMetrics struct {
	// BindTotal counts connection bind events by direction.
	// Label values: "fore", "back", "both".
	BindTotal *prometheus.CounterVec

	// UnbindTotal counts connection unbind events by reason.
	// Label values: "explicit", "disconnect", "session_destroy", "reaper".
	UnbindTotal *prometheus.CounterVec

	// BoundGauge tracks the number of bound connections per session.
	// Label: session_id (hex string).
	BoundGauge *prometheus.GaugeVec
}

// NewConnectionMetrics creates and registers connection metrics with the given
// Prometheus registerer. If reg is nil, metrics are created but not registered
// (useful for testing).
//
// On re-registration (server restart), existing collectors from the registry
// are reused so that metrics continue to be exported correctly.
func NewConnectionMetrics(reg prometheus.Registerer) *ConnectionMetrics {
	m := &ConnectionMetrics{
		BindTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_connections",
			Name:      "bind_total",
			Help:      "Total number of NFSv4.1 connection bind events",
		}, []string{"direction"}),
		UnbindTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_connections",
			Name:      "unbind_total",
			Help:      "Total number of NFSv4.1 connection unbind events",
		}, []string{"reason"}),
		BoundGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_connections",
			Name:      "bound",
			Help:      "Current number of bound connections per session",
		}, []string{"session_id"}),
	}

	if reg != nil {
		m.BindTotal = registerOrReuse(reg, m.BindTotal).(*prometheus.CounterVec)
		m.UnbindTotal = registerOrReuse(reg, m.UnbindTotal).(*prometheus.CounterVec)
		m.BoundGauge = registerOrReuse(reg, m.BoundGauge).(*prometheus.GaugeVec)
	}

	return m
}

// RecordBind increments the bind counter for the given direction.
func (m *ConnectionMetrics) RecordBind(direction string) {
	if m == nil {
		return
	}
	m.BindTotal.WithLabelValues(direction).Inc()
}

// RecordUnbind increments the unbind counter for the given reason.
func (m *ConnectionMetrics) RecordUnbind(reason string) {
	if m == nil {
		return
	}
	m.UnbindTotal.WithLabelValues(reason).Inc()
}

// SetBoundConnections sets the bound connection gauge for a session.
func (m *ConnectionMetrics) SetBoundConnections(sessionID string, count float64) {
	if m == nil {
		return
	}
	m.BoundGauge.WithLabelValues(sessionID).Set(count)
}

// RemoveSessionGauge removes the bound connection gauge label for a session.
// Called when a session is destroyed.
func (m *ConnectionMetrics) RemoveSessionGauge(sessionID string) {
	if m == nil {
		return
	}
	m.BoundGauge.DeleteLabelValues(sessionID)
}
