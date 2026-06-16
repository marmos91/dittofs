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
// All Record* methods are nil-receiver safe so call sites can invoke them
// unconditionally: a call site that holds no metrics handle (a nil *Metrics)
// simply no-ops. (The server itself builds the registry unconditionally and
// gates only the /metrics listener, but other callers/tests may pass nil.)
type instruments struct {
	requests     *prometheus.CounterVec   // {protocol,op,status}
	reqDuration  *prometheus.HistogramVec // {protocol,op}
	connsTotal   *prometheus.CounterVec   // {protocol}
	connsClosed  *prometheus.CounterVec   // {protocol}
	authAttempts *prometheus.CounterVec   // {protocol,mechanism}
	authFailures *prometheus.CounterVec   // {protocol,mechanism}

	// Local-store eviction / backpressure (subsystem "localstore").
	backpressureTotal       prometheus.Counter
	backpressureWaitSeconds prometheus.Histogram
	evictionsTotal          prometheus.Counter
	evictedBytesTotal       prometheus.Counter

	// Block-store garbage collection (subsystem "gc").
	gcRuns         *prometheus.CounterVec // {result}
	gcRunning      prometheus.Gauge
	gcLastRunTime  prometheus.Gauge
	gcSweptObjects prometheus.Counter
	gcFreedBytes   prometheus.Counter
	gcDurationSecs prometheus.Histogram

	// Snapshot / restore (subsystem "snapshot"). Only the event-driven
	// operation count + duration live here; the held-snapshot count
	// (snapshot_active) and last-success timestamp are already surfaced —
	// per-share and restart-safe — by the read-through collector, so they are
	// deliberately NOT duplicated as inline gauges (a second descriptor with
	// the same fqName would also fail registration).
	snapOps      *prometheus.CounterVec   // {op,result}
	snapDuration *prometheus.HistogramVec // {op}
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

		backpressureTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "localstore", Name: "backpressure_total",
			Help: "Times a write stalled waiting for the local cache to free space.",
		}),
		backpressureWaitSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "localstore", Name: "backpressure_wait_seconds",
			Help:    "Duration a write stalled under local-cache backpressure, in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
		evictionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "localstore", Name: "evictions_total",
			Help: "Local-cache CAS chunks evicted to reclaim space.",
		}),
		evictedBytesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "localstore", Name: "evicted_bytes_total",
			Help: "Bytes reclaimed by local-cache eviction.",
		}),

		gcRuns: factory(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "gc", Name: "runs_total",
			Help: "Block-store GC passes completed, by result (ok|error).",
		}, []string{"result"}),
		gcRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "gc", Name: "running",
			Help: "1 while a block-store GC pass is in progress, 0 otherwise.",
		}),
		gcLastRunTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "gc", Name: "last_run_timestamp_seconds",
			Help: "Unix time of the last completed block-store GC pass.",
		}),
		gcSweptObjects: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "gc", Name: "swept_objects_total",
			Help: "CAS objects reaped by block-store GC.",
		}),
		gcFreedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "gc", Name: "freed_bytes_total",
			Help: "Bytes freed by block-store GC.",
		}),
		gcDurationSecs: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "gc", Name: "duration_seconds",
			Help:    "Block-store GC pass duration, in seconds.",
			Buckets: prometheus.DefBuckets,
		}),

		snapOps: factory(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "snapshot", Name: "operations_total",
			Help: "Snapshot operations, by op (create|delete|restore) and result (ok|error).",
		}, []string{"op", "result"}),
		snapDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "snapshot", Name: "duration_seconds",
			Help:    "Snapshot operation duration in seconds, by op (create|delete|restore).",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
	}
	reg.MustRegister(
		in.requests, in.reqDuration, in.connsTotal, in.connsClosed, in.authAttempts, in.authFailures,
		in.backpressureTotal, in.backpressureWaitSeconds, in.evictionsTotal, in.evictedBytesTotal,
		in.gcRuns, in.gcRunning, in.gcLastRunTime, in.gcSweptObjects, in.gcFreedBytes, in.gcDurationSecs,
		in.snapOps, in.snapDuration,
	)
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

// RecordBackpressure records one write stall under local-cache backpressure and
// the duration it waited for space.
func (m *Metrics) RecordBackpressure(d time.Duration) {
	if m == nil {
		return
	}
	m.in.backpressureTotal.Inc()
	m.in.backpressureWaitSeconds.Observe(d.Seconds())
}

// RecordEviction records one evicted local-cache chunk and the bytes it
// reclaimed. Cheap (two atomic Incs) — safe on the write hot path.
func (m *Metrics) RecordEviction(bytes int64) {
	if m == nil {
		return
	}
	m.in.evictionsTotal.Inc()
	if bytes > 0 {
		m.in.evictedBytesTotal.Add(float64(bytes))
	}
}

// GCStarted marks a block-store GC pass as in progress. Pair with GCFinished.
//
// running is incremented (not Set to 1) so it stays correct under concurrent
// passes: RunBlockGC and RunBlockGCForShare are independent operator-triggered
// endpoints that may overlap. The gauge then reads the number of in-flight
// passes — 0 when idle, >=1 while any pass runs — which preserves the
// "1 while in progress, 0 otherwise" contract for the common single-pass case
// and avoids a premature Set(0) clearing the gauge out from under a concurrent
// pass. A pass that panics/exits without its GCFinished defer leaves the gauge
// elevated, which still surfaces the stuck/incomplete pass.
func (m *Metrics) GCStarted() {
	if m == nil {
		return
	}
	m.in.gcRunning.Inc()
}

// GCFinished records the completion of a block-store GC pass: its result
// ("ok"|"error"), reaped objects, freed bytes, and duration. It also decrements
// the running gauge and stamps the last-run timestamp.
func (m *Metrics) GCFinished(result string, sweptObjects, freedBytes int64, d time.Duration) {
	if m == nil {
		return
	}
	m.in.gcRunning.Dec()
	m.in.gcRuns.WithLabelValues(result).Inc()
	if sweptObjects > 0 {
		m.in.gcSweptObjects.Add(float64(sweptObjects))
	}
	if freedBytes > 0 {
		m.in.gcFreedBytes.Add(float64(freedBytes))
	}
	m.in.gcDurationSecs.Observe(d.Seconds())
	m.in.gcLastRunTime.Set(float64(time.Now().Unix()))
}

// RecordSnapshotOp records one snapshot operation: its count (by op and result)
// and its duration. op is "create"|"delete"|"restore"; result is "ok"|"error".
func (m *Metrics) RecordSnapshotOp(op, result string, d time.Duration) {
	if m == nil {
		return
	}
	m.in.snapOps.WithLabelValues(op, result).Inc()
	m.in.snapDuration.WithLabelValues(op).Observe(d.Seconds())
}
