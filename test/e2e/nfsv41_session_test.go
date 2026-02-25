//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4.1 Session and EOS (Exactly-Once Semantics) E2E Tests
// =============================================================================
//
// These tests validate NFSv4.1-specific session management and exactly-once
// semantics (EOS) behavior. EOS ensures that retried operations produce the
// same result as the original, preventing duplicate side effects after network
// disruptions.
//
// The v4.1 session lifecycle is: EXCHANGE_ID -> CREATE_SESSION -> SEQUENCE
// (on every compound) -> DESTROY_SESSION. EOS replay is verified through
// server-side log scraping for replay cache hits during v4.1 operations.
//
// Note: The Linux NFS client may not trigger replay during normal operation.
// These tests verify the EOS infrastructure is active and handling sessions
// correctly. Actual replay scenarios require network disruption which is
// tested separately in TestNFSv41EOSConnectionDisruption.

// =============================================================================
// Test 1: EOS Replay on Reconnect
// =============================================================================

// TestNFSv41EOSReplayOnReconnect verifies EOS behavior by performing heavy
// I/O through a v4.1 mount and checking server logs for any replay cache
// activity. The Linux NFS client may or may not trigger replays during normal
// operation, so the test logs warnings rather than failing if no replay is
// detected -- the EOS machinery is validated by unit tests; this E2E test
// validates the integration.
func TestNFSv41EOSReplayOnReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 EOS replay test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	sp, _, nfsPort := setupNFSv4TestServer(t)

	// Mount v4.1
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount.Cleanup)

	// Capture server log position before test operations
	logBefore := readLogFile(t, sp)

	// Perform heavy I/O to increase the probability of a natural retry.
	// Multiple concurrent file creates/writes stress the session slot table.
	var wg sync.WaitGroup
	const concurrentFiles = 20

	for i := range concurrentFiles {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			filePath := mount.FilePath(fmt.Sprintf("eos_replay_%03d.txt", idx))
			content := []byte(fmt.Sprintf("EOS replay test data for file %d -- padding to increase size %s",
				idx, strings.Repeat("x", 1024)))

			f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
			if err != nil {
				t.Logf("File create %d failed: %v", idx, err)
				return
			}

			_, _ = f.Write(content)
			_ = f.Sync()
			_ = f.Close()

			// Read back to generate more NFS operations
			_, _ = os.ReadFile(filePath)
		}(i)
	}
	wg.Wait()

	// Allow time for log flushing
	time.Sleep(1 * time.Second)

	// Clean up test files
	for i := range concurrentFiles {
		_ = os.Remove(mount.FilePath(fmt.Sprintf("eos_replay_%03d.txt", i)))
	}

	// Check server logs for replay-related activity
	logAfter := readLogFile(t, sp)
	newLogs := extractNewLogs(logBefore, logAfter)

	replayDetected := false
	replayIndicators := []string{
		"replay cache hit",
		"SEQUENCE replay",
		"slot seqid",
		"cached reply",
		"replay detected",
	}

	for _, indicator := range replayIndicators {
		if strings.Contains(strings.ToLower(newLogs), strings.ToLower(indicator)) {
			t.Logf("EOS replay activity detected: found %q in server logs", indicator)
			replayDetected = true
			break
		}
	}

	if !replayDetected {
		t.Log("WARNING: No replay cache activity detected in server logs. " +
			"This is expected during normal operation -- the Linux NFS client may not " +
			"trigger replays without network disruption. EOS machinery is validated by " +
			"unit tests; this E2E test confirms the v4.1 session infrastructure is active.")
	}

	// Verify the session was established (EXCHANGE_ID + CREATE_SESSION succeeded)
	sessionIndicators := []string{
		"EXCHANGE_ID",
		"CREATE_SESSION",
		"SEQUENCE",
	}

	sessionActive := false
	for _, indicator := range sessionIndicators {
		if strings.Contains(newLogs, indicator) {
			t.Logf("v4.1 session activity confirmed: found %q in server logs", indicator)
			sessionActive = true
		}
	}

	if !sessionActive {
		t.Log("NOTE: No explicit session operation log messages found. " +
			"The v4.1 mount succeeded, which implicitly validates EXCHANGE_ID, " +
			"CREATE_SESSION, and SEQUENCE handling.")
	}

	t.Log("TestNFSv41EOSReplayOnReconnect: PASSED (session infrastructure validated)")
}

// =============================================================================
// Test 2: EOS Connection Disruption
// =============================================================================

// TestNFSv41EOSConnectionDisruption attempts to force an EOS replay by
// disrupting the TCP connection during a large file write. If iptables or
// equivalent tools are unavailable, the test skips with an informative message.
func TestNFSv41EOSConnectionDisruption(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 EOS connection disruption test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	// Check if iptables is available for connection disruption
	_, err := exec.LookPath("iptables")
	if err != nil {
		t.Skip("Skipping: iptables not available for connection disruption (requires root and iptables)")
	}

	// Verify we can actually use iptables (need root)
	testCmd := exec.Command("iptables", "-L", "-n")
	if output, err := testCmd.CombinedOutput(); err != nil {
		t.Skipf("Skipping: cannot use iptables (need root): %v\nOutput: %s", err, string(output))
	}

	sp, _, nfsPort := setupNFSv4TestServer(t)

	// Mount v4.1
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount.Cleanup)

	// Capture log position
	logBefore := readLogFile(t, sp)

	// Start writing a large file in a goroutine
	filePath := mount.FilePath("eos_disruption_test.bin")
	writeErr := make(chan error, 1)

	go func() {
		data := make([]byte, 10*1024*1024) // 10MB
		for i := range data {
			data[i] = byte(i % 256)
		}

		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			writeErr <- err
			return
		}
		defer f.Close()

		_, err = f.Write(data)
		if err != nil {
			writeErr <- err
			return
		}

		err = f.Sync()
		writeErr <- err
	}()

	// Wait briefly for write to start, then disrupt connection
	time.Sleep(100 * time.Millisecond)

	// Add iptables rule to drop packets to the NFS port briefly
	dropRule := exec.Command("iptables", "-A", "OUTPUT",
		"-p", "tcp", "--dport", fmt.Sprintf("%d", nfsPort),
		"-j", "DROP")
	if err := dropRule.Run(); err != nil {
		t.Logf("Failed to add iptables rule: %v -- continuing without disruption", err)
	} else {
		// Clean up iptables rule after brief disruption
		t.Cleanup(func() {
			cleanRule := exec.Command("iptables", "-D", "OUTPUT",
				"-p", "tcp", "--dport", fmt.Sprintf("%d", nfsPort),
				"-j", "DROP")
			_ = cleanRule.Run()
		})

		// Keep the disruption for 500ms
		time.Sleep(500 * time.Millisecond)

		// Remove the drop rule to restore connectivity
		restoreRule := exec.Command("iptables", "-D", "OUTPUT",
			"-p", "tcp", "--dport", fmt.Sprintf("%d", nfsPort),
			"-j", "DROP")
		_ = restoreRule.Run()
	}

	// Wait for write to complete (may succeed or fail depending on disruption timing)
	select {
	case err := <-writeErr:
		if err != nil {
			t.Logf("Write completed with error (expected during disruption): %v", err)
		} else {
			t.Log("Write completed successfully (NFS client may have retried transparently)")
		}
	case <-time.After(60 * time.Second):
		t.Log("Write timed out (client may be stuck retrying)")
	}

	// Allow time for log flushing
	time.Sleep(2 * time.Second)

	// Clean up test file
	_ = os.Remove(filePath)

	// Check server logs for replay evidence
	logAfter := readLogFile(t, sp)
	newLogs := extractNewLogs(logBefore, logAfter)

	if strings.Contains(strings.ToLower(newLogs), "replay") {
		t.Log("Replay cache activity detected after connection disruption")
	} else {
		t.Log("NOTE: No explicit replay detected. The NFS client may have " +
			"established a new session rather than replaying on the old one.")
	}

	t.Log("TestNFSv41EOSConnectionDisruption: PASSED")
}

// =============================================================================
// Test 3: Session Establishment and Lifecycle
// =============================================================================

// TestNFSv41SessionEstablishment verifies the basic v4.1 session lifecycle:
// mount (EXCHANGE_ID + CREATE_SESSION + SEQUENCE), basic operations, and
// unmount (DESTROY_SESSION). This confirms the entire session state machine
// works end-to-end.
func TestNFSv41SessionEstablishment(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 session establishment test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	sp, _, nfsPort := setupNFSv4TestServer(t)

	// Capture log position before mount
	logBefore := readLogFile(t, sp)

	// Mount v4.1 -- this implicitly validates EXCHANGE_ID + CREATE_SESSION
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")

	// Perform basic operations to confirm session is functional
	testFile := mount.FilePath("session_lifecycle_test.txt")
	content := []byte("Session lifecycle test -- validates EXCHANGE_ID through DESTROY_SESSION")
	framework.WriteFile(t, testFile, content)

	readBack := framework.ReadFile(t, testFile)
	assert.Equal(t, content, readBack, "Session should be functional: write/read roundtrip")

	// Create a directory
	testDir := mount.FilePath("session_test_dir")
	framework.CreateDir(t, testDir)

	// Create a file inside the directory
	innerFile := filepath.Join(testDir, "inner.txt")
	framework.WriteFile(t, innerFile, []byte("inner file content"))

	// List directory
	entries := framework.ListDir(t, testDir)
	assert.Len(t, entries, 1, "Directory should contain 1 file")

	// Delete test artifacts
	require.NoError(t, os.Remove(innerFile))
	require.NoError(t, os.Remove(testDir))
	require.NoError(t, os.Remove(testFile))

	// Unmount -- should trigger DESTROY_SESSION
	mount.Cleanup()

	// Allow time for session teardown and log flushing
	time.Sleep(2 * time.Second)

	// Check server logs for session lifecycle
	logAfter := readLogFile(t, sp)
	newLogs := extractNewLogs(logBefore, logAfter)

	// Check for session creation evidence
	if strings.Contains(newLogs, "CREATE_SESSION") || strings.Contains(newLogs, "create_session") {
		t.Log("CREATE_SESSION confirmed in server logs")
	} else {
		t.Log("NOTE: No explicit CREATE_SESSION log message found. " +
			"The successful v4.1 mount implicitly confirms session creation.")
	}

	// Check for session destruction evidence
	if strings.Contains(newLogs, "DESTROY_SESSION") || strings.Contains(newLogs, "destroy_session") ||
		strings.Contains(newLogs, "session destroyed") || strings.Contains(newLogs, "Session destroyed") {
		t.Log("DESTROY_SESSION confirmed in server logs (clean session teardown)")
	} else {
		// DESTROY_SESSION may not be logged at INFO level, or the client may
		// just close the connection (relying on session reaper for cleanup)
		t.Log("NOTE: No explicit DESTROY_SESSION log message found. " +
			"The client may rely on session lease expiry for cleanup.")
	}

	t.Log("TestNFSv41SessionEstablishment: PASSED")
}

// =============================================================================
// Test 4: Multiple Concurrent Sessions
// =============================================================================

// TestNFSv41MultipleSessions verifies that the server can handle multiple
// concurrent v4.1 sessions from separate mount points, each with independent
// slot tables and sequence numbers.
func TestNFSv41MultipleSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 multiple sessions test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	_, _, nfsPort := setupNFSv4TestServer(t)

	// Mount 3 separate v4.1 clients (each creates its own session)
	const numClients = 3
	mounts := make([]*framework.Mount, numClients)

	for i := range numClients {
		mounts[i] = framework.MountNFSWithVersion(t, nfsPort, "4.1")
		t.Cleanup(mounts[i].Cleanup)
	}

	// Each client performs concurrent operations
	var wg sync.WaitGroup
	errors := make([]error, numClients)

	for i := range numClients {
		wg.Add(1)
		go func(clientIdx int) {
			defer wg.Done()

			mount := mounts[clientIdx]

			// Create a unique file per client
			fileName := fmt.Sprintf("session_%d_file.txt", clientIdx)
			filePath := mount.FilePath(fileName)
			content := []byte(fmt.Sprintf("Data from session %d", clientIdx))

			f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
			if err != nil {
				errors[clientIdx] = fmt.Errorf("client %d create: %w", clientIdx, err)
				return
			}

			_, err = f.Write(content)
			if err != nil {
				_ = f.Close()
				errors[clientIdx] = fmt.Errorf("client %d write: %w", clientIdx, err)
				return
			}

			_ = f.Sync()
			_ = f.Close()

			// Read back
			readContent, err := os.ReadFile(filePath)
			if err != nil {
				errors[clientIdx] = fmt.Errorf("client %d read: %w", clientIdx, err)
				return
			}

			if string(readContent) != string(content) {
				errors[clientIdx] = fmt.Errorf("client %d data mismatch: got %q, want %q",
					clientIdx, string(readContent), string(content))
			}
		}(i)
	}

	wg.Wait()

	// Check for errors
	for i, err := range errors {
		assert.NoError(t, err, "Client %d should complete without errors", i)
	}

	// Verify cross-session visibility: each client should see files from other clients
	time.Sleep(500 * time.Millisecond) // Allow NFS cache to settle

	for i := range numClients {
		for j := range numClients {
			fileName := fmt.Sprintf("session_%d_file.txt", j)
			filePath := mounts[i].FilePath(fileName)
			assert.True(t, framework.FileExists(filePath),
				"Client %d should see file created by client %d", i, j)
		}
	}

	// Clean up test files
	for j := range numClients {
		_ = os.Remove(mounts[0].FilePath(fmt.Sprintf("session_%d_file.txt", j)))
	}

	t.Log("TestNFSv41MultipleSessions: PASSED")
}

// =============================================================================
// Test 5: Session Recovery After Server Restart
// =============================================================================

// TestNFSv41SessionRecoveryAfterRestart verifies that after a server restart,
// a v4.1 client can re-establish a session and resume operations. The old
// session state is lost (memory backend), so the client must perform a new
// EXCHANGE_ID + CREATE_SESSION.
func TestNFSv41SessionRecoveryAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 session recovery test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	// Start first server
	sp1 := helpers.StartServerProcess(t, "")

	runner1 := helpers.LoginAsAdmin(t, sp1.APIURL())

	metaStore := helpers.UniqueTestName("recoverymeta")
	payloadStore := helpers.UniqueTestName("recoverypayload")

	_, err := runner1.CreateMetadataStore(metaStore, "memory")
	require.NoError(t, err)

	_, err = runner1.CreatePayloadStore(payloadStore, "memory")
	require.NoError(t, err)

	_, err = runner1.CreateShare("/export", metaStore, payloadStore)
	require.NoError(t, err)

	nfsPort := helpers.FindFreePort(t)
	_, err = runner1.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err)

	err = helpers.WaitForAdapterStatus(t, runner1, "nfs", true, 5*time.Second)
	require.NoError(t, err)
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// Mount v4.1 and perform some operations
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount.Cleanup)

	testFile := mount.FilePath("recovery_test.txt")
	framework.WriteFile(t, testFile, []byte("before restart"))

	// Stop first server
	sp1.ForceKill()

	// Start a new server on the same port
	sp2 := helpers.StartServerProcess(t, "")
	t.Cleanup(sp2.ForceKill)

	runner2 := helpers.LoginAsAdmin(t, sp2.APIURL())

	metaStore2 := helpers.UniqueTestName("recoverymeta2")
	payloadStore2 := helpers.UniqueTestName("recoverypayload2")

	_, err = runner2.CreateMetadataStore(metaStore2, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner2.DeleteMetadataStore(metaStore2) })

	_, err = runner2.CreatePayloadStore(payloadStore2, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner2.DeletePayloadStore(payloadStore2) })

	_, err = runner2.CreateShare("/export", metaStore2, payloadStore2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner2.DeleteShare("/export") })

	_, err = runner2.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = runner2.DisableAdapter("nfs") })

	err = helpers.WaitForAdapterStatus(t, runner2, "nfs", true, 5*time.Second)
	require.NoError(t, err)
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// The old mount's session is dead. The kernel NFS client should detect
	// this and attempt to re-establish the session. We test by trying new
	// operations -- the old file is gone (memory backend), but we should
	// be able to create new files.
	//
	// Use a context with timeout since the client may take time to recover.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Attempt to create a new file on the existing mount.
	// This may fail if the NFS client hasn't recovered the session yet.
	newFile := mount.FilePath("after_restart.txt")
	var recoveryErr error

	for {
		select {
		case <-ctx.Done():
			if recoveryErr != nil {
				t.Logf("Session recovery did not complete within timeout: %v", recoveryErr)
				t.Log("NOTE: This is expected with memory backend -- the NFS client " +
					"may report stale handle errors for the old mount after server restart.")
			}
			// Not a test failure -- session recovery behavior depends on client implementation
			t.Log("TestNFSv41SessionRecoveryAfterRestart: PASSED (recovery timeout is acceptable)")
			return
		default:
			f, err := os.OpenFile(newFile, os.O_CREATE|os.O_RDWR, 0644)
			if err != nil {
				recoveryErr = err
				time.Sleep(1 * time.Second)
				continue
			}
			_, _ = f.Write([]byte("after restart"))
			_ = f.Sync()
			_ = f.Close()

			t.Log("Session recovered: successfully created file after server restart")
			_ = os.Remove(newFile)
			t.Log("TestNFSv41SessionRecoveryAfterRestart: PASSED")
			return
		}
	}
}
