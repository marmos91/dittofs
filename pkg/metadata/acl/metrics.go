package acl

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ACLMetrics tracks Prometheus metrics for ACL operations.
//
// All metrics use the "dittofs_acl_" prefix. Methods handle nil receiver
// gracefully, so a nil *ACLMetrics acts as a no-op (zero overhead when
// metrics are disabled). This follows the pattern from GSSMetrics.
//
// Metrics tracked:
//   - ACL evaluation duration (histogram)
//   - ACL evaluation totals by result (allowed/denied)
//   - ACL evaluation deny count
//   - ACL inheritance computation duration (histogram)
//   - ACL validation error count
type ACLMetrics struct {
	// EvaluationDuration tracks time to evaluate an ACL for an access decision.
	EvaluationDuration prometheus.Histogram

	// EvaluationTotal counts total ACL evaluations by result.
	// Labels: result=[allowed, denied]
	EvaluationTotal *prometheus.CounterVec

	// EvaluationDenyTotal counts ACL evaluations that resulted in denial.
	EvaluationDenyTotal prometheus.Counter

	// InheritanceDuration tracks time to compute inherited ACL for new files/dirs.
	InheritanceDuration prometheus.Histogram

	// ValidationErrorsTotal counts ACL validation failures (bad ordering, too many ACEs).
	ValidationErrorsTotal prometheus.Counter
}

var (
	aclMetricsOnce     sync.Once
	aclMetricsInstance *ACLMetrics
)

// NewACLMetrics creates and registers ACL Prometheus metrics.
//
// If registerer is nil, prometheus.DefaultRegisterer is used.
// This function is idempotent - uses sync.Once to ensure metrics are
// registered exactly once, even if called multiple times.
//
// Parameters:
//   - registerer: Prometheus registerer (nil = DefaultRegisterer)
//
// Returns:
//   - *ACLMetrics: Configured metrics struct with all metrics registered
func NewACLMetrics(registerer prometheus.Registerer) *ACLMetrics {
	aclMetricsOnce.Do(func() {
		if registerer == nil {
			registerer = prometheus.DefaultRegisterer
		}

		m := &ACLMetrics{
			EvaluationDuration: prometheus.NewHistogram(
				prometheus.HistogramOpts{
					Name:    "dittofs_acl_evaluation_duration_seconds",
					Help:    "Time to evaluate an ACL for an access decision",
					Buckets: prometheus.DefBuckets,
				},
			),
			EvaluationTotal: prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "dittofs_acl_evaluation_total",
					Help: "Total ACL evaluations by result",
				},
				[]string{"result"},
			),
			EvaluationDenyTotal: prometheus.NewCounter(
				prometheus.CounterOpts{
					Name: "dittofs_acl_evaluation_deny_total",
					Help: "Total ACL evaluations that resulted in denial",
				},
			),
			InheritanceDuration: prometheus.NewHistogram(
				prometheus.HistogramOpts{
					Name:    "dittofs_acl_inheritance_duration_seconds",
					Help:    "Time to compute inherited ACL for new files/directories",
					Buckets: prometheus.DefBuckets,
				},
			),
			ValidationErrorsTotal: prometheus.NewCounter(
				prometheus.CounterOpts{
					Name: "dittofs_acl_validation_errors_total",
					Help: "Total ACL validation failures (bad ordering, too many ACEs)",
				},
			),
		}

		// Register all metrics
		registerer.MustRegister(
			m.EvaluationDuration,
			m.EvaluationTotal,
			m.EvaluationDenyTotal,
			m.InheritanceDuration,
			m.ValidationErrorsTotal,
		)

		aclMetricsInstance = m
	})

	return aclMetricsInstance
}

// ObserveEvaluation records an ACL evaluation result with its duration.
//
// Parameters:
//   - duration: Time taken for the evaluation
//   - allowed: Whether the access was granted
func (m *ACLMetrics) ObserveEvaluation(duration time.Duration, allowed bool) {
	if m == nil {
		return
	}
	m.EvaluationDuration.Observe(duration.Seconds())
	if allowed {
		m.EvaluationTotal.WithLabelValues("allowed").Inc()
	} else {
		m.EvaluationTotal.WithLabelValues("denied").Inc()
		m.EvaluationDenyTotal.Inc()
	}
}

// ObserveInheritance records the duration of an ACL inheritance computation.
//
// Parameters:
//   - duration: Time taken for the inheritance computation
func (m *ACLMetrics) ObserveInheritance(duration time.Duration) {
	if m == nil {
		return
	}
	m.InheritanceDuration.Observe(duration.Seconds())
}

// ObserveValidationError records an ACL validation failure.
func (m *ACLMetrics) ObserveValidationError() {
	if m == nil {
		return
	}
	m.ValidationErrorsTotal.Inc()
}
