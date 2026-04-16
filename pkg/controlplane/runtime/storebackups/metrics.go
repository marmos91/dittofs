// Metrics + tracing hooks for backup/restore operations (Plan 05-09 D-19, D-20).
//
// Ships the minimal observability Phase 5 requires: one counter per terminal
// state and one last-success gauge per (repo_id, kind). A single OTel span
// wraps each RunBackup / RunRestore invocation — enough for an operator to
// alert on silent-failure (Pitfall #10) without pulling in the full
// Prometheus suite.
//
// The `MetricsCollector` interface and the concrete `OTelTracer` are active
// in the default build. The concrete Prometheus implementation is
// deliberately NOT shipped in Phase 5 because `prometheus/client_golang` is
// not in `go.mod`; per Plan 05-09 guardrail, Phase 5 must not introduce new
// top-level dependencies. Phase 7 (or whichever plan promotes Prometheus to
// a direct dep) adds a `PromMetrics` implementation alongside this
// interface. Until then, operators that wire a concrete collector from
// outside this package satisfy the same contract.
package storebackups

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Metric name constants — exported as the observable contract operators
// alert against. Names chosen per Plan 05-09 D-19.
const (
	// MetricBackupOperationsTotal is the counter of terminal backup/restore
	// outcomes, labelled {kind, outcome}. kind ∈ {"backup","restore"}.
	// outcome ∈ {"succeeded","failed","interrupted"}.
	MetricBackupOperationsTotal = "backup_operations_total"

	// MetricBackupLastSuccessTimestampSeconds is the gauge holding the Unix
	// timestamp (seconds) of the most recent successful backup/restore per
	// (repo_id, kind). Operators alert when `time() - value > 2 * schedule`.
	MetricBackupLastSuccessTimestampSeconds = "backup_last_success_timestamp_seconds"

	// OTel span operation names (Plan 05-09 D-19).
	SpanBackupRun  = "backup.run"
	SpanRestoreRun = "restore.run"
)

// Outcome string constants — keep in lock-step with MetricBackupOperationsTotal
// label cardinality.
const (
	OutcomeSucceeded   = "succeeded"
	OutcomeFailed      = "failed"
	OutcomeInterrupted = "interrupted"

	KindBackup  = "backup"
	KindRestore = "restore"
)

// MetricsCollector is the minimal observability hook Phase 5 ships (D-19).
// Implementations may register Prometheus metrics, forward to a third-party
// collector, or be a noop.
type MetricsCollector interface {
	// RecordOutcome increments backup_operations_total{kind, outcome}.
	// Called exactly once per terminal state per RunBackup/RunRestore.
	RecordOutcome(kind, outcome string)
	// RecordLastSuccess updates backup_last_success_timestamp_seconds.
	// Called ONLY on successful outcomes; failed/interrupted leave the
	// gauge at its previous value so alert rules fire correctly.
	RecordLastSuccess(repoID, kind string, at time.Time)
}

// NoopMetrics is the zero-overhead collector used when server.metrics.enabled
// is false (D-20). It is the default so RunBackup/RunRestore never nil-deref.
type NoopMetrics struct{}

// RecordOutcome is a no-op.
func (NoopMetrics) RecordOutcome(kind, outcome string) {}

// RecordLastSuccess is a no-op.
func (NoopMetrics) RecordLastSuccess(repoID, kind string, at time.Time) {}

// Tracer is the minimal OTel hook (D-19). Implementations return a ctx +
// finish func; callers defer finish() to end the span.
type Tracer interface {
	// Start begins a span named `operation` inheriting from ctx. The
	// returned context carries the span, and the finish func ends the span
	// and records err as the span's error status when non-nil.
	Start(ctx context.Context, operation string) (context.Context, func(err error))
}

// NoopTracer satisfies Tracer for telemetry-disabled boots (D-20).
type NoopTracer struct{}

// Start returns ctx unchanged and a no-op finish.
func (NoopTracer) Start(ctx context.Context, operation string) (context.Context, func(err error)) {
	return ctx, func(error) {}
}

// OTelTracer adapts an OpenTelemetry Tracer to the Tracer interface. Wire
// via `NewOTelTracer(otel.Tracer("dittofs.backup"))` at server startup,
// guarded by the existing `telemetry.enabled` config flag.
type OTelTracer struct {
	t trace.Tracer
}

// NewOTelTracer wraps an OTel Tracer. If t is nil, returns a tracer that
// behaves as NoopTracer to spare callers the nil check.
func NewOTelTracer(t trace.Tracer) *OTelTracer {
	return &OTelTracer{t: t}
}

// Start begins an OTel span named `operation` as a child of the span in ctx
// (if any). The returned finish func records err on the span if non-nil and
// then ends the span.
func (o *OTelTracer) Start(ctx context.Context, operation string) (context.Context, func(err error)) {
	if o == nil || o.t == nil {
		return ctx, func(error) {}
	}
	ctx, span := o.t.Start(ctx, operation)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}
}
