//go:build e2e

package framework

import (
	"os"
	"path/filepath"
	"testing"
)

// With DITTOFS_E2E_BIN set, a binary comes from there, and a missing one is
// an error; unset, the caller builds it.
func TestPrebuilt(t *testing.T) {
	t.Setenv("DITTOFS_E2E_BIN", "")
	if _, ok, _ := Prebuilt("dfs"); ok {
		t.Error("unset, it didn't leave the build to the caller")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dfs"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DITTOFS_E2E_BIN", dir)
	if path, ok, err := Prebuilt("dfs"); !ok || err != nil || path != filepath.Join(dir, "dfs") {
		t.Errorf("dfs: %q, %t, %v", path, ok, err)
	}
	if _, ok, err := Prebuilt("dfsctl"); !ok || err == nil {
		t.Errorf("a missing dfsctl: %t, %v", ok, err)
	}
}

// The network-namespace tests' binaries come from DITTOFS_E2E_BIN too.
func TestBuildE2EBinaryPrebuilt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dfsctl"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DITTOFS_E2E_BIN", dir)
	if got := buildE2EBinary(t, t.TempDir(), "./cmd/dfsctl", "dfsctl"); got != filepath.Join(dir, "dfsctl") {
		t.Errorf("buildE2EBinary: %s", got)
	}
}
