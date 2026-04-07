package nfs_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/health"
)

func minimalEnabledConfig() nfs.NFSConfig {
	return nfs.NFSConfig{
		Enabled: true,
		Port:    12049, // ephemeral high port; we never call Serve
	}
}

func TestNFSAdapter_Healthcheck_DisabledWhenConfigOff(t *testing.T) {
	cfg := minimalEnabledConfig()
	cfg.Enabled = false
	a := nfs.New(cfg, nil)

	rep := a.Healthcheck(context.Background())
	if rep.Status != health.StatusDisabled {
		t.Fatalf("disabled config: got %q (%q), want disabled", rep.Status, rep.Message)
	}
	if rep.Message == "" {
		t.Fatal("expected non-empty message describing the disabled state")
	}
}

func TestNFSAdapter_Healthcheck_UnknownBeforeStart(t *testing.T) {
	a := nfs.New(minimalEnabledConfig(), nil)
	rep := a.Healthcheck(context.Background())
	// The base adapter has not been started, so the override delegates
	// and returns unknown.
	if rep.Status != health.StatusUnknown {
		t.Fatalf("enabled but not started: got %q (%q), want unknown", rep.Status, rep.Message)
	}
}
