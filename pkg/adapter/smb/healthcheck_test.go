package smb_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter/smb"
	"github.com/marmos91/dittofs/pkg/health"
)

func minimalEnabledConfig() smb.Config {
	return smb.Config{
		Enabled: true,
		Port:    1445, // unprivileged port; we never call Serve
	}
}

func TestSMBAdapter_Healthcheck_DisabledWhenConfigOff(t *testing.T) {
	cfg := minimalEnabledConfig()
	cfg.Enabled = false
	a := smb.New(cfg)

	rep := a.Healthcheck(context.Background())
	if rep.Status != health.StatusDisabled {
		t.Fatalf("disabled config: got %q (%q), want disabled", rep.Status, rep.Message)
	}
	if rep.Message == "" {
		t.Fatal("expected non-empty message describing the disabled state")
	}
}

func TestSMBAdapter_Healthcheck_UnknownBeforeStart(t *testing.T) {
	a := smb.New(minimalEnabledConfig())
	rep := a.Healthcheck(context.Background())
	if rep.Status != health.StatusUnknown {
		t.Fatalf("enabled but not started: got %q (%q), want unknown", rep.Status, rep.Message)
	}
}
