// Observability hook tests (Plan 05-09 D-19, D-20).
//
// Covers the terminal-state behavior of MetricsCollector + Tracer when wired
// onto RunBackup and RunRestore. Uses the restoreHarness defined in
// restore_test.go and adds a minimal fake collector/tracer pair.
package storebackups

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeMetrics records every RecordOutcome + RecordLastSuccess call for
// assertions. Mutex-guarded so the tests can safely read the slices.
type fakeMetrics struct {
	mu            sync.Mutex
	outcomes      []string // kind|outcome
	lastSuccesses []string // repoID|kind|unix
}

func (f *fakeMetrics) RecordOutcome(kind, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, fmt.Sprintf("%s|%s", kind, outcome))
}

func (f *fakeMetrics) RecordLastSuccess(repoID, kind string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSuccesses = append(f.lastSuccesses, fmt.Sprintf("%s|%s|%d", repoID, kind, at.Unix()))
}

func (f *fakeMetrics) snapshot() (outcomes, lastSuccesses []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.outcomes...), append([]string(nil), f.lastSuccesses...)
}

// fakeTracer counts Start calls with their operation name.
type fakeTracer struct {
	mu         sync.Mutex
	starts     []string
	finishes   []string // operation|hasError
	finishErrs []error
}

func (f *fakeTracer) Start(ctx context.Context, operation string) (context.Context, func(err error)) {
	f.mu.Lock()
	f.starts = append(f.starts, operation)
	f.mu.Unlock()
	return ctx, func(err error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		hasErr := "nil"
		if err != nil {
			hasErr = "err"
		}
		f.finishes = append(f.finishes, fmt.Sprintf("%s|%s", operation, hasErr))
		f.finishErrs = append(f.finishErrs, err)
	}
}

func (f *fakeTracer) snapshot() (starts, finishes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.starts...), append([]string(nil), f.finishes...)
}

// TestRunRestore_Observability_FailureClassified — a pre-flight failure
// (shares still enabled) must increment backup_operations_total
// {kind=restore, outcome=failed} and MUST NOT update the last-success gauge.
// OTel span restore.run must be opened and closed.
func TestRunRestore_Observability_FailureClassified(t *testing.T) {
	h := newRestoreHarness(t, map[string][]string{
		"default-meta": {"/foo"}, // one enabled share → pre-flight fail
	})
	metrics := &fakeMetrics{}
	tracer := &fakeTracer{}
	WithMetricsCollector(metrics)(h.svc)
	WithTracer(tracer)(h.svc)

	err := h.svc.RunRestore(context.Background(), h.repoID, nil)
	if err == nil {
		t.Fatal("expected pre-flight error, got nil")
	}
	if !errors.Is(err, ErrRestorePreconditionFailed) {
		t.Fatalf("expected ErrRestorePreconditionFailed, got %v", err)
	}

	outcomes, lastSuccesses := metrics.snapshot()
	wantOutcome := "restore|failed"
	if len(outcomes) != 1 || outcomes[0] != wantOutcome {
		t.Errorf("outcomes = %v, want [%s]", outcomes, wantOutcome)
	}
	if len(lastSuccesses) != 0 {
		t.Errorf("last_success updated on failure: %v", lastSuccesses)
	}

	starts, finishes := tracer.snapshot()
	if len(starts) != 1 || starts[0] != SpanRestoreRun {
		t.Errorf("starts = %v, want [%s]", starts, SpanRestoreRun)
	}
	if len(finishes) != 1 || finishes[0] != "restore.run|err" {
		t.Errorf("finishes = %v, want [restore.run|err]", finishes)
	}
}

// TestRunRestore_Observability_InterruptedClassified — a ctx.Canceled error
// must classify as outcome=interrupted (not failed).
func TestRunRestore_Observability_InterruptedClassified(t *testing.T) {
	h := newRestoreHarness(t, nil)
	metrics := &fakeMetrics{}
	tracer := &fakeTracer{}
	WithMetricsCollector(metrics)(h.svc)
	WithTracer(tracer)(h.svc)

	// Seed one succeeded record so record selection succeeds, but cancel the
	// context so Destination.GetManifestOnly returns ctx.Err(). The fake
	// destination wraps ctx.Err() via fmt.Errorf — we force a pristine
	// context.Canceled by cancelling before invoking RunRestore.
	h.seedSucceededRecord(t, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Pre-flight passes (no enabled shares); we rely on downstream to see
	// ctx.Canceled. Inject the failure via destination factory returning
	// context.Canceled directly — the cleanest way to exercise the
	// interrupted classifier without plumbing cancellation through the
	// fake destination.
	h.dst.manifestErrToReturn = context.Canceled

	err := h.svc.RunRestore(ctx, h.repoID, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	outcomes, _ := metrics.snapshot()
	if len(outcomes) != 1 || outcomes[0] != "restore|interrupted" {
		t.Errorf("outcomes = %v, want [restore|interrupted]", outcomes)
	}
}

// TestRunBackup_Observability_FailureClassified — RunBackup with a resolver
// error surfaces outcome=failed and the backup.run span is opened/closed.
func TestRunBackup_Observability_FailureClassified(t *testing.T) {
	h := newRestoreHarness(t, nil)
	// Break the resolver so RunBackup fails.
	h.resolver.err = errors.New("resolver busted")

	metrics := &fakeMetrics{}
	tracer := &fakeTracer{}
	WithMetricsCollector(metrics)(h.svc)
	WithTracer(tracer)(h.svc)

	_, err := h.svc.RunBackup(context.Background(), h.repoID)
	if err == nil {
		t.Fatal("expected resolver error, got nil")
	}

	outcomes, lastSuccesses := metrics.snapshot()
	if len(outcomes) != 1 || outcomes[0] != "backup|failed" {
		t.Errorf("outcomes = %v, want [backup|failed]", outcomes)
	}
	if len(lastSuccesses) != 0 {
		t.Errorf("last_success updated on failure: %v", lastSuccesses)
	}

	starts, finishes := tracer.snapshot()
	if len(starts) != 1 || starts[0] != SpanBackupRun {
		t.Errorf("starts = %v, want [%s]", starts, SpanBackupRun)
	}
	if len(finishes) != 1 || finishes[0] != "backup.run|err" {
		t.Errorf("finishes = %v, want [backup.run|err]", finishes)
	}
}

// TestClassifyOutcome — covers the {nil, context.Canceled,
// context.DeadlineExceeded, other} branches.
func TestClassifyOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, OutcomeSucceeded},
		{"canceled", context.Canceled, OutcomeInterrupted},
		{"deadline", context.DeadlineExceeded, OutcomeInterrupted},
		{"wrapped_canceled", fmt.Errorf("wrap: %w", context.Canceled), OutcomeInterrupted},
		{"other", errors.New("boom"), OutcomeFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyOutcome(c.err); got != c.want {
				t.Errorf("classifyOutcome(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestNoopCollectors — verify NoopMetrics and NoopTracer don't panic and
// return no-op finishers. Guard rail for the default-wiring code path.
func TestNoopCollectors(t *testing.T) {
	var m MetricsCollector = NoopMetrics{}
	m.RecordOutcome("backup", "succeeded")
	m.RecordLastSuccess("repo-1", "backup", time.Now())

	var tr Tracer = NoopTracer{}
	ctx, finish := tr.Start(context.Background(), "backup.run")
	if ctx == nil {
		t.Error("NoopTracer.Start returned nil ctx")
	}
	// Must not panic on either nil or non-nil err.
	finish(nil)
	finish(errors.New("err"))
}

// TestOTelTracer_NilSafe — NewOTelTracer(nil) returns a tracer whose Start
// is a no-op (so callers that optionally wire an OTel tracer don't need to
// special-case nil).
func TestOTelTracer_NilSafe(t *testing.T) {
	tr := NewOTelTracer(nil)
	ctx, finish := tr.Start(context.Background(), "backup.run")
	if ctx == nil {
		t.Error("OTelTracer(nil).Start returned nil ctx")
	}
	finish(nil) // must not panic
}
