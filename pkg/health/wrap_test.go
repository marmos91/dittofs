package health

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReportFromError_NilError(t *testing.T) {
	rep := ReportFromError(nil, 12*time.Millisecond)
	if rep.Status != StatusHealthy {
		t.Fatalf("status: got %q, want healthy", rep.Status)
	}
	if rep.Message != "" {
		t.Fatalf("message: got %q, want empty", rep.Message)
	}
	if rep.LatencyMs != 12 {
		t.Fatalf("latency: got %d ms, want 12", rep.LatencyMs)
	}
	if rep.CheckedAt.IsZero() {
		t.Fatal("CheckedAt should be populated")
	}
}

func TestReportFromError_WithError(t *testing.T) {
	rep := ReportFromError(errors.New("disk full"), 3*time.Millisecond)
	if rep.Status != StatusUnhealthy {
		t.Fatalf("status: got %q, want unhealthy", rep.Status)
	}
	if rep.Message != "disk full" {
		t.Fatalf("message: got %q, want 'disk full'", rep.Message)
	}
	if rep.LatencyMs != 3 {
		t.Fatalf("latency: got %d ms, want 3", rep.LatencyMs)
	}
}

func TestCheckerFromErrorFunc_HealthyDelegates(t *testing.T) {
	called := false
	var seenCtx context.Context
	c := CheckerFromErrorFunc(func(ctx context.Context) error {
		called = true
		seenCtx = ctx
		return nil
	})
	rep := c.Healthcheck(context.Background())
	if !called {
		t.Fatal("inner probe was not called")
	}
	if seenCtx == nil {
		t.Fatal("inner probe did not receive a context")
	}
	if rep.Status != StatusHealthy {
		t.Fatalf("status: got %q, want healthy", rep.Status)
	}
}

func TestCheckerFromErrorFunc_UnhealthyOnError(t *testing.T) {
	c := CheckerFromErrorFunc(func(ctx context.Context) error {
		return errors.New("connection refused")
	})
	rep := c.Healthcheck(context.Background())
	if rep.Status != StatusUnhealthy {
		t.Fatalf("status: got %q, want unhealthy", rep.Status)
	}
	if rep.Message != "connection refused" {
		t.Fatalf("message: got %q, want 'connection refused'", rep.Message)
	}
}

func TestCheckerFromErrorFunc_MeasuresLatency(t *testing.T) {
	c := CheckerFromErrorFunc(func(ctx context.Context) error {
		// Small sleep drives a non-zero latency measurement; the
		// exact duration is not asserted because loaded CI runners
		// make tight timing windows flaky.
		time.Sleep(2 * time.Millisecond)
		return nil
	})
	rep := c.Healthcheck(context.Background())
	if rep.LatencyMs < 1 {
		t.Fatalf("latency: got %d ms, expected at least 1", rep.LatencyMs)
	}
}

// TestReportFromError_ContextCanceledMapsToUnknown verifies the
// Checker contract: a probe aborted by the caller's context (canceled
// or deadline exceeded) must surface as StatusUnknown rather than
// StatusUnhealthy. The probe was indeterminate, not the entity broken.
func TestReportFromError_ContextCanceledMapsToUnknown(t *testing.T) {
	rep := ReportFromError(context.Canceled, 5*time.Millisecond)
	if rep.Status != StatusUnknown {
		t.Fatalf("context.Canceled: got %q, want unknown", rep.Status)
	}
}

func TestReportFromError_DeadlineExceededMapsToUnknown(t *testing.T) {
	rep := ReportFromError(context.DeadlineExceeded, 5*time.Millisecond)
	if rep.Status != StatusUnknown {
		t.Fatalf("context.DeadlineExceeded: got %q, want unknown", rep.Status)
	}
}

// TestReportFromError_WrappedContextErrorMapsToUnknown ensures the
// errors.Is unwrapping works for context errors that have been
// fmt.Errorf'd with %w by an upstream layer (e.g. the S3 driver
// wrapping HeadBucket errors).
func TestReportFromError_WrappedContextErrorMapsToUnknown(t *testing.T) {
	wrapped := fmt.Errorf("S3 head bucket: %w", context.Canceled)
	rep := ReportFromError(wrapped, 5*time.Millisecond)
	if rep.Status != StatusUnknown {
		t.Fatalf("wrapped context.Canceled: got %q, want unknown", rep.Status)
	}
}

func TestNewUnknownReport(t *testing.T) {
	rep := NewUnknownReport("probe interrupted", 7*time.Millisecond)
	if rep.Status != StatusUnknown {
		t.Fatalf("status: got %q, want unknown", rep.Status)
	}
	if rep.Message != "probe interrupted" {
		t.Fatalf("message: got %q, want 'probe interrupted'", rep.Message)
	}
	if rep.LatencyMs != 7 {
		t.Fatalf("latency: got %d ms, want 7", rep.LatencyMs)
	}
}
