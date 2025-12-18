//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
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
		// Cleanup on interrupt
		framework.CleanupAllContexts()      // Clean up cached test contexts (mounts, servers)
		framework.CleanupSharedContainers() // Clean up Docker containers
		cancel()
		os.Exit(1)
	}()

	// Cleanup any stale mounts from previous failed runs before starting
	framework.CleanupStaleMounts()

	// Run tests
	code := m.Run()

	// Cleanup after all tests complete
	framework.CleanupAllContexts()      // Clean up cached test contexts (mounts, servers)
	framework.CleanupSharedContainers() // Clean up Docker containers

	// Exit with context awareness
	select {
	case <-ctx.Done():
		os.Exit(1)
	default:
		os.Exit(code)
	}
}
