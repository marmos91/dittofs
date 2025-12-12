//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// TestMain handles setup and cleanup for all E2E tests
func TestMain(m *testing.M) {
	// Setup signal handler for graceful shutdown on CTRL+C
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		// Cleanup containers on interrupt
		cleanupSharedContainers()
		cancel()
		os.Exit(1)
	}()

	// Run tests
	code := m.Run()

	// Cleanup after all tests complete
	cleanupSharedContainers()

	// Exit with context awareness
	select {
	case <-ctx.Done():
		os.Exit(1)
	default:
		os.Exit(code)
	}
}

// cleanupSharedContainers terminates all shared test containers
func cleanupSharedContainers() {
	ctx := context.Background()

	// Cleanup PostgreSQL container
	if sharedPostgresHelper != nil && sharedPostgresHelper.Container != nil {
		_ = sharedPostgresHelper.Container.Terminate(ctx)
		sharedPostgresHelper = nil
	}

	// Cleanup Localstack container
	if sharedLocalstackHelper != nil && sharedLocalstackHelper.Container != nil {
		_ = sharedLocalstackHelper.Container.Terminate(ctx)
		sharedLocalstackHelper = nil
	}
}
