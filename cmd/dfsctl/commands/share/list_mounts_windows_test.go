//go:build windows

package share

import (
	"testing"
)

func TestGetDittoFSMounts_NoError(t *testing.T) {
	// getDittoFSMounts runs "net use" which is always available on Windows.
	// Even with no mounts, the command should succeed and return an empty slice.
	mounts, err := getDittoFSMounts()
	if err != nil {
		t.Fatalf("getDittoFSMounts() returned unexpected error: %v", err)
	}

	// We cannot assert the exact contents since it depends on the host,
	// but the result must be a valid (possibly empty) slice.
	if mounts == nil {
		// nil is acceptable (no localhost mounts found), but verify it is not
		// due to an error we missed.
		t.Log("getDittoFSMounts() returned nil (no localhost mounts found)")
	} else {
		t.Logf("getDittoFSMounts() found %d mount(s)", len(mounts))
		for _, m := range mounts {
			if m.Source == "" {
				t.Error("mount entry has empty Source")
			}
			if m.Protocol == "" {
				t.Error("mount entry has empty Protocol")
			}
		}
	}
}
