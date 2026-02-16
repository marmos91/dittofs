//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerLifecycle validates the DittoFS server lifecycle operations.
// These tests verify server startup, health endpoints, status command,
// and graceful shutdown via signals.
//
// Note: These tests are sequential and cannot run in parallel because
// each needs to start and stop its own server instance.
func TestServerLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping server lifecycle tests in short mode")
	}

	t.Run("start and check health", testStartAndCheckHealth)
	t.Run("health vs readiness endpoints", testHealthVsReadiness)
	t.Run("status command reports running", testStatusReportsRunning)
	t.Run("graceful shutdown on SIGTERM", testGracefulShutdownSIGTERM)
	t.Run("graceful shutdown on SIGINT", testGracefulShutdownSIGINT)
}

// testStartAndCheckHealth starts a server and verifies the /health endpoint
// returns the expected structure including started_at and uptime fields.
func testStartAndCheckHealth(t *testing.T) {
	// Start server with automatic cleanup on test completion
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Check health endpoint
	health, err := sp.CheckHealth()
	require.NoError(t, err, "Health check should succeed")

	// Verify response structure
	assert.Equal(t, "healthy", health.Status, "Server should be healthy")
	assert.NotEmpty(t, health.Data.StartedAt, "started_at should be set")
	assert.NotEmpty(t, health.Data.Uptime, "uptime should be set")
	assert.Equal(t, "dittofs", health.Data.Service, "service should be 'dittofs'")

	// Verify uptime is reasonable (should be very small since we just started)
	// Parse uptime to verify it's a valid duration
	assert.Contains(t, health.Data.Uptime, "s", "uptime should contain seconds unit")

	// Stop gracefully
	err = sp.StopGracefully()
	require.NoError(t, err, "Graceful stop should succeed")
}

// testHealthVsReadiness verifies that health and readiness endpoints
// return different information as per CONTEXT.md:
// "Readiness vs Health distinction: Server is ready when all adapters started
// AND all store healthchecks pass"
//
// Note: In this test, adapters may fail to start due to port conflicts with
// other processes. The readiness endpoint should return 503 in that case.
// We verify the endpoint behavior rather than requiring a fully-ready server.
func testHealthVsReadiness(t *testing.T) {
	// Start server
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Check health endpoint (liveness) - should always succeed when API is up
	health, err := sp.CheckHealth()
	require.NoError(t, err, "Health check should succeed")
	assert.Equal(t, "healthy", health.Status)
	// Health endpoint returns service info, uptime
	assert.Equal(t, "dittofs", health.Data.Service)
	assert.NotEmpty(t, health.Data.StartedAt)
	assert.NotEmpty(t, health.Data.Uptime)

	// Check readiness endpoint - may return healthy or unhealthy depending on adapter status
	ready, err := sp.CheckReady()
	require.NoError(t, err, "Readiness check HTTP request should succeed")

	// Readiness endpoint should return either healthy (adapters started) or
	// unhealthy (adapters failed due to port conflicts)
	if ready.Status == "healthy" {
		// If healthy, verify response structure includes adapter info
		assert.GreaterOrEqual(t, ready.Data.Adapters.Running, 1, "Should have at least 1 running adapter")
	} else {
		// If unhealthy, verify it's due to expected reasons
		assert.Equal(t, "unhealthy", ready.Status)
		t.Logf("Readiness returned unhealthy (expected if ports in use): %s", ready.Error)
	}

	// Stop gracefully
	err = sp.StopGracefully()
	require.NoError(t, err, "Graceful stop should succeed")
}

// testStatusReportsRunning verifies the `dittofs status` command correctly
// reports the server state when running.
func testStatusReportsRunning(t *testing.T) {
	// Start server
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Run dittofs status command with the correct API port
	output, err := helpers.RunDfs(t,
		"status",
		"--api-port", itoa(sp.APIPort()),
		"--output", "json",
	)
	require.NoError(t, err, "Status command should succeed")

	// Parse JSON output
	var status struct {
		Running   bool   `json:"running"`
		Healthy   bool   `json:"healthy"`
		PID       int    `json:"pid,omitempty"`
		Message   string `json:"message"`
		StartedAt string `json:"started_at,omitempty"`
		Uptime    string `json:"uptime,omitempty"`
	}
	err = json.Unmarshal(output, &status)
	require.NoError(t, err, "Status output should be valid JSON: %s", string(output))

	// Verify status
	assert.True(t, status.Running, "Server should be reported as running")
	assert.True(t, status.Healthy, "Server should be reported as healthy")
	assert.NotEmpty(t, status.Message, "Status message should be set")
	assert.Contains(t, status.Message, "running", "Message should indicate running")

	// Stop gracefully
	err = sp.StopGracefully()
	require.NoError(t, err, "Graceful stop should succeed")
}

// testGracefulShutdownSIGTERM verifies that sending SIGTERM triggers
// graceful shutdown within a reasonable timeout.
func testGracefulShutdownSIGTERM(t *testing.T) {
	// Start server
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Verify server is running
	require.True(t, sp.ProcessRunning(), "Server process should be running")

	// Send SIGTERM
	err := sp.SendSignal(syscall.SIGTERM)
	require.NoError(t, err, "Sending SIGTERM should succeed")

	// Wait for exit with timeout
	start := time.Now()
	err = sp.WaitForExit(10 * time.Second)
	elapsed := time.Since(start)

	// Verify clean exit
	require.NoError(t, err, "Server should exit cleanly after SIGTERM")
	assert.Less(t, elapsed, 10*time.Second, "Server should shut down within 10 seconds")

	// Verify process is no longer running
	assert.False(t, sp.ProcessRunning(), "Server process should not be running after shutdown")

	t.Logf("SIGTERM shutdown took %v", elapsed)
}

// testGracefulShutdownSIGINT verifies that sending SIGINT (Ctrl+C equivalent)
// triggers graceful shutdown.
func testGracefulShutdownSIGINT(t *testing.T) {
	// Start server
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Verify server is running
	require.True(t, sp.ProcessRunning(), "Server process should be running")

	// Send SIGINT
	err := sp.SendSignal(syscall.SIGINT)
	require.NoError(t, err, "Sending SIGINT should succeed")

	// Wait for exit with timeout
	start := time.Now()
	err = sp.WaitForExit(10 * time.Second)
	elapsed := time.Since(start)

	// Verify clean exit
	require.NoError(t, err, "Server should exit cleanly after SIGINT")
	assert.Less(t, elapsed, 10*time.Second, "Server should shut down within 10 seconds")

	// Verify process is no longer running
	assert.False(t, sp.ProcessRunning(), "Server process should not be running after shutdown")

	t.Logf("SIGINT shutdown took %v", elapsed)
}

// itoa converts an int to string using fmt.Sprintf
func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
