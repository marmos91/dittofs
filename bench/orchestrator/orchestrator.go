package orchestrator

import (
	"context"
	"fmt"
)

// RunOutput is what a WorkloadRunner returns for one workload: the measured
// numbers and any pprof files it captured. Errors are returned out-of-band.
type RunOutput struct {
	Metrics      Metrics
	ProfilePaths []string
}

// WorkloadRunner executes a single workload and returns its measured output.
// The orchestrator injects this so it stays free of the engine/pprof/runtime
// machinery: cmd/bench wires a runner that composes the bench/blockstore
// engine and the pprof envelope; tests inject a fast in-memory fake. A non-nil
// error marks the workload as failed (Outcome != completed) without aborting
// the rest of the manifest.
type WorkloadRunner func(ctx context.Context, p WorkloadParams) (RunOutput, error)

// Run executes every workload in the manifest in order, recording per-workload
// results into a freshly stamped Document. Run metadata (run ID, timestamp, git
// SHA, system) is supplied by the caller — Run never calls time.Now() or reads
// the environment itself, which keeps it deterministic under test.
//
// A workload runner error is recorded as a per-workload failure and the run
// continues; the document outcome becomes "partial". If the manifest itself is
// invalid the run is "aborted" before any workload executes.
func Run(ctx context.Context, m Manifest, runID, timestamp, gitSHA string, sys System, run WorkloadRunner) (*Document, error) {
	doc := NewDocument(runID, timestamp, gitSHA, sys)

	if err := m.Validate(); err != nil {
		doc.Outcome = OutcomeAborted
		doc.AbortReason = err.Error()
		return doc, err
	}
	if run == nil {
		doc.Outcome = OutcomeAborted
		doc.AbortReason = "no workload runner provided"
		return doc, fmt.Errorf("orchestrator.Run: runner is nil")
	}

	anyFailed := false
	for _, p := range m.Workloads {
		// Honor cancellation between workloads so a budget/timeout halts the
		// run cleanly with the results gathered so far.
		if err := ctx.Err(); err != nil {
			doc.Outcome = OutcomeAborted
			doc.AbortReason = err.Error()
			return doc, err
		}

		out, err := run(ctx, p)
		if err != nil {
			anyFailed = true
			doc.Workloads[p.Name] = WorkloadResult{
				Outcome: OutcomePartial,
				Error:   err.Error(),
				Params:  p,
			}
			continue
		}
		metrics := out.Metrics
		doc.Workloads[p.Name] = WorkloadResult{
			Outcome:      OutcomeCompleted,
			Params:       p,
			Metrics:      &metrics,
			ProfilePaths: out.ProfilePaths,
		}
	}

	if anyFailed {
		doc.Outcome = OutcomePartial
	}
	return doc, nil
}

// MetricsFromRun derives the schema Metrics from a raw duration/ops/bytes
// triple. Shared by the real runner so the ns/op and throughput math lives in
// one place. durationNs must be > 0.
func MetricsFromRun(durationNs, ops, bytes int64) Metrics {
	m := Metrics{
		DurationNs: durationNs,
		Ops:        ops,
		Bytes:      bytes,
	}
	if durationNs > 0 {
		secs := float64(durationNs) / 1e9
		m.OpsPerSec = float64(ops) / secs
		m.BytesPerSec = float64(bytes) / secs
	}
	if ops > 0 {
		m.NsPerOp = float64(durationNs) / float64(ops)
	}
	return m
}
