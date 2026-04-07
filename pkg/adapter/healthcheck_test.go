package adapter

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/health"
)

// newTestBaseAdapter creates a BaseAdapter that has not yet been
// started. Tests can flip the started flag and Shutdown channel
// directly to exercise the various Healthcheck branches without
// going through the real ServeWithFactory listener loop.
func newTestBaseAdapter(t *testing.T, protocol string) *BaseAdapter {
	t.Helper()
	return NewBaseAdapter(BaseConfig{}, protocol)
}

func TestBaseAdapter_Healthcheck_UnknownBeforeStart(t *testing.T) {
	b := newTestBaseAdapter(t, "TEST")
	rep := b.Healthcheck(context.Background())
	if rep.Status != health.StatusUnknown {
		t.Fatalf("not started: got %q (%q), want unknown", rep.Status, rep.Message)
	}
	if rep.Message == "" {
		t.Fatal("expected non-empty message describing the not-started state")
	}
	if rep.CheckedAt.IsZero() {
		t.Fatal("CheckedAt should be populated")
	}
}

func TestBaseAdapter_Healthcheck_HealthyAfterStart(t *testing.T) {
	b := newTestBaseAdapter(t, "TEST")
	b.started.Store(true)

	rep := b.Healthcheck(context.Background())
	if rep.Status != health.StatusHealthy {
		t.Fatalf("started: got %q (%q), want healthy", rep.Status, rep.Message)
	}
}

func TestBaseAdapter_Healthcheck_UnhealthyOnceShutdown(t *testing.T) {
	b := newTestBaseAdapter(t, "TEST")
	b.started.Store(true)

	// Simulate shutdown by closing the Shutdown channel via initiateShutdown.
	// Calling initiateShutdown twice is safe (sync.Once).
	b.initiateShutdown()

	rep := b.Healthcheck(context.Background())
	if rep.Status != health.StatusUnhealthy {
		t.Fatalf("shutdown: got %q (%q), want unhealthy", rep.Status, rep.Message)
	}
	if rep.Message == "" {
		t.Fatal("expected non-empty message describing the shutdown state")
	}
}
