package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// instruments holds the inline, event-driven metrics (counters/histograms)
// incremented at call sites — distinct from the read-through collector, which
// emits existing state at scrape time. They are registered on the owned
// registry in New.
//
// All Record* methods are nil-receiver safe: when metrics are disabled no
// *Metrics is constructed, so call sites can invoke them unconditionally.
type instruments struct {
	requests     *prometheus.CounterVec   // {protocol,op,status}
	reqDuration  *prometheus.HistogramVec // {protocol,op}
	connsTotal   *prometheus.CounterVec   // {protocol}
	connsClosed  *prometheus.CounterVec   // {protocol}
	authAttempts *prometheus.CounterVec   // {protocol,mechanism}
	authFailures *prometheus.CounterVec   // {protocol,mechanism}
}

func newInstruments(reg *prometheus.Registry) *instruments {
	factory := prometheus.NewCounterVec
	in := &instruments{
		requests: factory(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "adapter", Name: "requests_total",
			Help: "Protocol requests handled, by protocol, operation, and status (ok|error).",
		}, []string{"protocol", "op", "status"}),
		reqDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "adapter", Name: "request_duration_seconds",
			Help:    "Protocol request handling latency in seconds, by protocol and operation.",
			Buckets: prometheus.DefBuckets,
		}, []string{"protocol", "op"}),
		connsTotal: factory(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "adapter", Name: "connections_total",
			Help: "Connections accepted since process start, by protocol.",
		}, []string{"protocol"}),
		connsClosed: factory(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "adapter", Name: "connections_closed_total",
			Help: "Connections closed since process start, by protocol. Active = total - closed.",
		}, []string{"protocol"}),
		authAttempts: factory(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "auth", Name: "attempts_total",
			Help: "Authentication attempts, by protocol and mechanism (sys|krb5|ntlm).",
		}, []string{"protocol", "mechanism"}),
		authFailures: factory(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "auth", Name: "failures_total",
			Help: "Failed authentication attempts, by protocol and mechanism (sys|krb5|ntlm).",
		}, []string{"protocol", "mechanism"}),
	}
	reg.MustRegister(in.requests, in.reqDuration, in.connsTotal, in.connsClosed, in.authAttempts, in.authFailures)
	return in
}

// RecordRequest records one handled protocol request: its count (by status) and
// its latency. status should be "ok" or "error".
func (m *Metrics) RecordRequest(protocol, op, status string, d time.Duration) {
	if m == nil {
		return
	}
	m.in.requests.WithLabelValues(protocol, op, status).Inc()
	m.in.reqDuration.WithLabelValues(protocol, op).Observe(d.Seconds())
}

// RecordConnAccepted counts an accepted connection.
func (m *Metrics) RecordConnAccepted(protocol string) {
	if m == nil {
		return
	}
	m.in.connsTotal.WithLabelValues(protocol).Inc()
}

// RecordConnClosed counts a closed connection.
func (m *Metrics) RecordConnClosed(protocol string) {
	if m == nil {
		return
	}
	m.in.connsClosed.WithLabelValues(protocol).Inc()
}

// RecordAuth records an authentication attempt and, when ok is false, a failure.
func (m *Metrics) RecordAuth(protocol, mechanism string, ok bool) {
	if m == nil {
		return
	}
	m.in.authAttempts.WithLabelValues(protocol, mechanism).Inc()
	if !ok {
		m.in.authFailures.WithLabelValues(protocol, mechanism).Inc()
	}
}
