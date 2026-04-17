package blockstoreprobe

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/health"
)

// setHome redirects os.UserHomeDir to a tempdir for the duration of
// the test. On Windows, os.UserHomeDir reads USERPROFILE, not HOME,
// so skip there rather than silently pass an unredirected lookup.
func setHome(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tilde expansion test uses HOME redirection (not applicable on Windows)")
	}
	t.Setenv("HOME", dir)
}

func fsBlockStore(t *testing.T, path string) *models.BlockStoreConfig {
	t.Helper()
	bs := &models.BlockStoreConfig{
		Name: "test-local",
		Kind: models.BlockStoreKindLocal,
		Type: "fs",
	}
	if err := bs.SetConfig(map[string]any{"path": path}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	return bs
}

// TestProbeLocal_TildeExpansion covers the #391 regression: a leading
// ~ in the configured path must be expanded before the probe stats it,
// otherwise a valid local store reports unhealthy.
func TestProbeLocal_TildeExpansion(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	abs := filepath.Join(home, "localstore")
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", abs, err)
	}

	rep := Probe(context.Background(), fsBlockStore(t, "~/localstore"))
	if rep.Status != health.StatusHealthy {
		t.Fatalf("tilde path probe: status=%q message=%q, want healthy", rep.Status, rep.Message)
	}
}

// TestProbeLocal_TildeExpansion_MissingDir verifies the existing
// "configured path does not exist" message is preserved when the
// expanded path genuinely does not exist.
func TestProbeLocal_TildeExpansion_MissingDir(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	rep := Probe(context.Background(), fsBlockStore(t, "~/does-not-exist"))
	if rep.Status != health.StatusUnhealthy {
		t.Fatalf("missing tilde path: status=%q, want unhealthy", rep.Status)
	}
	if !strings.Contains(rep.Message, "does not exist") {
		t.Errorf("missing tilde path: message=%q, want contains 'does not exist'", rep.Message)
	}
}
