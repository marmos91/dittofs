package blockstoreprobe

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/health"
)

func TestProbe_NilConfigIsUnhealthy(t *testing.T) {
	got := Probe(context.Background(), nil)
	if got.Status != health.StatusUnhealthy {
		t.Errorf("status = %v, want unhealthy for a nil config", got.Status)
	}
}

func TestProbe_MemoryStoreIsHealthy(t *testing.T) {
	got := Probe(context.Background(), &models.BlockStoreConfig{Name: "mem", Type: "memory"})
	if got.Status != health.StatusHealthy {
		t.Errorf("status = %v (%s), want healthy", got.Status, got.Message)
	}
}

// An unrecognised type must fail closed rather than be reported healthy: the
// probe is what tells an operator their store is reachable.
func TestProbe_UnknownTypeIsUnhealthy(t *testing.T) {
	got := Probe(context.Background(), &models.BlockStoreConfig{Name: "odd", Type: "fs"})
	if got.Status != health.StatusUnhealthy {
		t.Errorf("status = %v, want unhealthy for an unsupported type", got.Status)
	}
}
