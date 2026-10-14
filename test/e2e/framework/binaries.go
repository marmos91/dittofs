//go:build e2e

package framework

import (
	"fmt"
	"os"
	"path/filepath"
)

// Prebuilt returns $DITTOFS_E2E_BIN/name when DITTOFS_E2E_BIN is set: dt
// builds dfs and dfsctl there as the user before it runs the suite as root,
// so root never runs go build. ok is false when the variable isn't set, and
// the caller builds the binary itself.
func Prebuilt(name string) (path string, ok bool, err error) {
	dir := os.Getenv("DITTOFS_E2E_BIN")
	if dir == "" {
		return "", false, nil
	}
	path = filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		return "", true, fmt.Errorf("%s from DITTOFS_E2E_BIN: %w", name, err)
	}
	return path, true, nil
}
