package engine

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/health"
)

// TestBlockStore_Healthcheck_HealthyEndToEnd verifies the engine's new
// Healthcheck() method returns healthy when both local and (if present)
// remote stores are healthy. Uses the same fixture pattern as the
// existing health-monitor tests.
func TestBlockStore_Healthcheck_HealthyEndToEnd(t *testing.T) {
	bs, _ := newHealthTestEngine(t)
	defer func() { _ = bs.Close() }()

	rep := bs.Healthcheck(context.Background())
	if rep.Status != health.StatusHealthy {
		t.Fatalf("fresh engine: got %q (%q), want healthy", rep.Status, rep.Message)
	}
	if rep.CheckedAt.IsZero() {
		t.Fatal("CheckedAt should be populated")
	}
}

// TestBlockStore_Healthcheck_RespectsCanceledContext verifies the engine
// short-circuits on a canceled caller context rather than running
// downstream probes.
func TestBlockStore_Healthcheck_RespectsCanceledContext(t *testing.T) {
	bs, _ := newHealthTestEngine(t)
	defer func() { _ = bs.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep := bs.Healthcheck(ctx)
	if rep.Status != health.StatusUnknown {
		t.Fatalf("canceled ctx: got %q, want unknown", rep.Status)
	}
}
