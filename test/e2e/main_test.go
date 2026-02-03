//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
)

// Global test environment (shared containers)
// Containers are started lazily by first test that needs them.
// This variable provides access to the environment for cleanup coordination.
var testEnv *helpers.TestEnvironment

// TestMain handles setup and cleanup for all E2E tests.
// It sets up signal handlers for graceful shutdown and coordinates
// cleanup of Docker containers via the helpers package.
func TestMain(m *testing.M) {
	// Setup signal handler for graceful shutdown on CTRL+C
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		// Cleanup on interrupt - MUST clean mounts BEFORE containers
		// Otherwise we get stale mount dialogs on macOS
		framework.CleanupStaleMounts()
		if testEnv != nil {
			testEnv.Cleanup() // Clean up Docker containers via helpers
		}
		cancel()
		os.Exit(1)
	}()

	// Cleanup any stale mounts from previous failed runs before starting
	framework.CleanupStaleMounts()

	// Initialize test environment placeholder for cleanup coordination.
	// Containers are NOT started here - they are started lazily by
	// NewTestEnvironment(t) when individual tests need them.
	// This design is required because framework helpers need *testing.T.
	testEnv = helpers.NewTestEnvironmentForMain(ctx)

	// Run tests
	code := m.Run()

	// Cleanup after all tests complete
	// Clean mounts BEFORE containers to avoid stale mount dialogs
	framework.CleanupStaleMounts()
	if testEnv != nil {
		testEnv.Cleanup() // Clean up Docker containers
	}

	// Exit with context awareness
	select {
	case <-ctx.Done():
		os.Exit(1)
	default:
		os.Exit(code)
	}
}

// GetTestEnv returns the global test environment for use in tests.
// Tests should call env.NewScope(t) to get per-test isolation with
// unique Postgres schemas and S3 prefixes.
//
// Example usage:
//
//	func TestSomething(t *testing.T) {
//	    env := GetTestEnv()
//	    scope := env.NewScope(t)
//	    // scope.SchemaName() - unique Postgres schema
//	    // scope.S3Prefix() - unique S3 prefix
//	    // scope.DB() - scoped database connection
//	}
func GetTestEnv() *helpers.TestEnvironment {
	return testEnv
}
