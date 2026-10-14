//go:build e2e

package helpers

import (
	"os"
	"path/filepath"
	"testing"
)

// With DITTOFS_E2E_BIN set, the helpers take dfs and dfsctl from there
// rather than building them.
func TestPrebuiltBinaries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"dfs", "dfsctl"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DITTOFS_E2E_BIN", dir)
	if got := findDfsBinary(t); got != filepath.Join(dir, "dfs") {
		t.Errorf("findDfsBinary: %s", got)
	}
	if got := findDfsBinaryForCLI(t); got != filepath.Join(dir, "dfs") {
		t.Errorf("findDfsBinaryForCLI: %s", got)
	}
	if got := NewCLIRunner("http://127.0.0.1:1", "").getBinary(); got != filepath.Join(dir, "dfsctl") {
		t.Errorf("getBinary: %s", got)
	}
}
