package e2e

import (
	"os"
	"testing"
)

// TestUnmount tests unmounting the share
func TestUnmount(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create a file before unmount
		filePath := tc.Path("before_unmount.txt")
		err := os.WriteFile(filePath, []byte("test"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Unmount
		tc.unmountNFS()

		// Try to access the file (should fail)
		_, err = os.Stat(filePath)
		if err == nil {
			t.Errorf("Should not be able to access file after unmount")
		}

		// Remount for cleanup
		tc.mountNFS()
	})
}
