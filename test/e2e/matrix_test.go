package e2e

import (
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/suites"
)

// TestE2E is the main entry point for end-to-end tests
// It runs all test suites against all store type combinations
func TestE2E(t *testing.T) {
	// Check if NFS mounting is available
	if !framework.CanMount(t) {
		t.Skip("NFS mounting not available on this system")
	}

	// Define store types to test
	storeTypes := []framework.StoreType{
		framework.StoreTypeMemory,
		framework.StoreTypeFilesystem,
	}

	// Run all test suites for each store type
	for _, storeType := range storeTypes {
		storeType := storeType // capture for parallel tests
		t.Run(string(storeType), func(t *testing.T) {
			// Run test suites
			t.Run("BasicOperations", func(t *testing.T) {
				suites.TestBasicOperations(t, storeType)
			})

			t.Run("DirectoryOperations", func(t *testing.T) {
				suites.TestDirectoryOperations(t, storeType)
			})

			t.Run("SymlinkOperations", func(t *testing.T) {
				suites.TestSymlinkOperations(t, storeType)
			})

			t.Run("HardLinkOperations", func(t *testing.T) {
				suites.TestHardLinkOperations(t, storeType)
			})

			t.Run("FileAttributes", func(t *testing.T) {
				suites.TestFileAttributes(t, storeType)
			})

			t.Run("Idempotency", func(t *testing.T) {
				suites.TestIdempotency(t, storeType)
			})

			t.Run("EdgeCases", func(t *testing.T) {
				suites.TestEdgeCases(t, storeType)
			})
		})
	}
}
