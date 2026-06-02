package orchestrator

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

// fakeRunner returns deterministic metrics derived from the params, with no
// engine/pprof — so the run loop is tested in isolation and fast.
func fakeRunner(_ context.Context, p WorkloadParams) (RunOutput, error) {
	return RunOutput{
		Metrics:      MetricsFromRun(int64(p.Ops)*1000, int64(p.Ops), int64(p.Ops)*int64(p.BlockSize)),
		ProfilePaths: []string{p.Name + "/cpu.pprof"},
	}, nil
}

func twoWorkloadManifest() Manifest {
	return Manifest{Workloads: []WorkloadParams{
		{Name: "a", Workload: "sequential-write", Ops: 100, BlockSize: 4096, Seed: 1},
		{Name: "b", Workload: "random-write", Ops: 200, BlockSize: 4096, Seed: 2},
	}}
}

func TestRunHappyPath(t *testing.T) {
	doc, err := Run(context.Background(), twoWorkloadManifest(), "rid", "2026-01-02T15:04:05Z", "sha", System{}, fakeRunner)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if doc.Outcome != OutcomeCompleted {
		t.Errorf("outcome = %s, want completed", doc.Outcome)
	}
	if len(doc.Workloads) != 2 {
		t.Fatalf("want 2 workloads, got %d", len(doc.Workloads))
	}
	a := doc.Workloads["a"]
	if a.Outcome != OutcomeCompleted || a.Metrics == nil || a.Metrics.Ops != 100 {
		t.Errorf("workload a wrong: %+v", a)
	}
	if a.Params.Workload != "sequential-write" {
		t.Errorf("params not echoed: %+v", a.Params)
	}
	if len(a.ProfilePaths) != 1 {
		t.Errorf("profile paths not captured: %+v", a.ProfilePaths)
	}
}

func TestRunRecordsFailureAsPartial(t *testing.T) {
	var calls atomic.Int32
	runner := func(_ context.Context, p WorkloadParams) (RunOutput, error) {
		calls.Add(1)
		if p.Name == "a" {
			return RunOutput{}, fmt.Errorf("boom")
		}
		return fakeRunner(context.Background(), p)
	}
	doc, err := Run(context.Background(), twoWorkloadManifest(), "rid", "2026-01-02T15:04:05Z", "sha", System{}, runner)
	if err != nil {
		t.Fatalf("Run should not error on workload failure: %v", err)
	}
	if doc.Outcome != OutcomePartial {
		t.Errorf("outcome = %s, want partial", doc.Outcome)
	}
	// b still ran after a failed.
	if calls.Load() != 2 {
		t.Errorf("expected both workloads attempted, got %d calls", calls.Load())
	}
	if doc.Workloads["a"].Outcome != OutcomePartial || doc.Workloads["a"].Error == "" {
		t.Errorf("failed workload not recorded: %+v", doc.Workloads["a"])
	}
	if doc.Workloads["b"].Outcome != OutcomeCompleted {
		t.Errorf("surviving workload wrong: %+v", doc.Workloads["b"])
	}
}

func TestRunInvalidManifestAborts(t *testing.T) {
	doc, err := Run(context.Background(), Manifest{}, "rid", "2026-01-02T15:04:05Z", "sha", System{}, fakeRunner)
	if err == nil {
		t.Fatal("expected error for empty manifest")
	}
	if doc.Outcome != OutcomeAborted || doc.AbortReason == "" {
		t.Errorf("expected aborted with reason, got %+v", doc)
	}
}

func TestRunNilRunnerAborts(t *testing.T) {
	doc, err := Run(context.Background(), twoWorkloadManifest(), "rid", "ts", "sha", System{}, nil)
	if err == nil {
		t.Fatal("expected error for nil runner")
	}
	if doc.Outcome != OutcomeAborted {
		t.Errorf("outcome = %s, want aborted", doc.Outcome)
	}
}

func TestRunCanceledContextAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doc, err := Run(ctx, twoWorkloadManifest(), "rid", "ts", "sha", System{}, fakeRunner)
	if err == nil {
		t.Fatal("expected context error")
	}
	if doc.Outcome != OutcomeAborted {
		t.Errorf("outcome = %s, want aborted", doc.Outcome)
	}
}

func TestRunDeterministic(t *testing.T) {
	// Same inputs (including injected timestamp/seed) produce byte-identical
	// documents — no hidden time.Now()/rand.
	m := twoWorkloadManifest()
	d1, _ := Run(context.Background(), m, "rid", "2026-01-02T15:04:05Z", "sha", System{OS: "x"}, fakeRunner)
	d2, _ := Run(context.Background(), m, "rid", "2026-01-02T15:04:05Z", "sha", System{OS: "x"}, fakeRunner)
	b1, _ := d1.Marshal()
	b2, _ := d2.Marshal()
	if string(b1) != string(b2) {
		t.Errorf("non-deterministic output:\n%s\nvs\n%s", b1, b2)
	}
}
