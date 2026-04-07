package memory

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/health"
)

func TestMemoryStore_Healthcheck_HealthyOnFreshStore(t *testing.T) {
	s := New()
	defer s.Close()

	rep := s.Healthcheck(context.Background())
	if rep.Status != health.StatusHealthy {
		t.Fatalf("fresh store: got %q (%q), want healthy", rep.Status, rep.Message)
	}
	if rep.CheckedAt.IsZero() {
		t.Fatal("CheckedAt should be populated")
	}
}

func TestMemoryStore_Healthcheck_UnhealthyAfterClose(t *testing.T) {
	s := New()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rep := s.Healthcheck(context.Background())
	if rep.Status != health.StatusUnhealthy {
		t.Fatalf("closed store: got %q, want unhealthy", rep.Status)
	}
	if rep.Message == "" {
		t.Fatal("expected non-empty message describing closure")
	}
}

func TestMemoryStore_Healthcheck_RespectsCanceledContext(t *testing.T) {
	s := New()
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep := s.Healthcheck(ctx)
	if rep.Status != health.StatusUnhealthy {
		t.Fatalf("canceled ctx: got %q, want unhealthy", rep.Status)
	}
}
