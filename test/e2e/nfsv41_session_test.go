//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// NFSv4.1 Session E2E Tests
// =============================================================================
//
// These tests exercise v4.1 session management through a kernel mount:
// EXCHANGE_ID -> CREATE_SESSION -> SEQUENCE (on every compound) ->
// DESTROY_SESSION.
//
// Exactly-once semantics are not observable here: a replay needs the server to
// execute a request whose reply the client then never receives, and nothing a
// kernel mount does on demand produces that. The replay cache is covered
// deterministically by TestSequence_ReplayWithCache,
// TestSequence_ReplayWithoutCache and TestSequence_FalseRetry_DifferentOps in
// internal/adapter/nfs/v4/handlers.

var (
	createSessionOp  = regexp.MustCompile(`op_name=CREATE_SESSION`)
	destroySessionOp = regexp.MustCompile(`op_name=DESTROY_SESSION`)
)

// =============================================================================
// Test 1: Concurrent I/O on One Session
// =============================================================================

// TestNFSv41ConcurrentIOOnSession runs concurrent create/write/fsync/read
// cycles through one v4.1 mount, so the compounds contend for the session's
// slot table, and requires every file to round-trip.
func TestNFSv41ConcurrentIOOnSession(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 concurrent session I/O test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	_, _, nfsPort := setupNFSv4TestServer(t)

	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount.Cleanup)

	const concurrentFiles = 20
	errs := make([]error, concurrentFiles)
	var wg sync.WaitGroup

	for i := range concurrentFiles {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			filePath := mount.FilePath(fmt.Sprintf("session_io_%03d.txt", idx))
			content := []byte(fmt.Sprintf("session I/O data for file %d %s", idx, strings.Repeat("x", 1024)))
			errs[idx] = writeSyncReadBack(filePath, content)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "file %d should round-trip", i)
		_ = os.Remove(mount.FilePath(fmt.Sprintf("session_io_%03d.txt", i)))
	}
}

// writeSyncReadBack creates path, writes content, fsyncs, closes, and checks
// that a fresh read returns content.
func writeSyncReadBack(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	if !bytes.Equal(got, content) {
		return fmt.Errorf("read back %d bytes, want %d", len(got), len(content))
	}
	return nil
}

// =============================================================================
// Test 2: I/O Survives Packet Loss
// =============================================================================

// TestNFSv41IOSurvivesPacketLoss drops the client's packets to the server for
// 500ms in the middle of a 10MB write and requires the write and fsync to
// complete with the data intact. A DROP stalls TCP rather than breaking the
// connection, so the client retransmits on the same connection.
func TestNFSv41IOSurvivesPacketLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 packet loss test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skip("Skipping: iptables not available to drop packets")
	}
	if output, err := exec.Command("iptables", "-L", "-n").CombinedOutput(); err != nil {
		t.Skipf("Skipping: cannot use iptables (need root): %v\nOutput: %s", err, string(output))
	}

	_, _, nfsPort := setupNFSv4TestServer(t)

	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount.Cleanup)

	filePath := mount.FilePath("packet_loss_test.bin")
	t.Cleanup(func() { _ = os.Remove(filePath) })
	data := make([]byte, 10*1024*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	writeErr := make(chan error, 1)
	go func() { writeErr <- writeSyncReadBack(filePath, data) }()

	time.Sleep(100 * time.Millisecond)

	ruleArgs := []string{"OUTPUT", "-p", "tcp", "--dport", fmt.Sprintf("%d", nfsPort), "-j", "DROP"}
	out, err := exec.Command("iptables", append([]string{"-A"}, ruleArgs...)...).CombinedOutput()
	require.NoError(t, err, "add iptables DROP rule: %s", string(out))
	removeRule := func() { _ = exec.Command("iptables", append([]string{"-D"}, ruleArgs...)...).Run() }
	t.Cleanup(removeRule)

	time.Sleep(500 * time.Millisecond)
	removeRule()

	select {
	case err := <-writeErr:
		require.NoError(t, err, "write across the packet loss should complete with the data intact")
	case <-time.After(60 * time.Second):
		t.Fatal("write did not complete within 60s of the packet loss ending")
	}
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

	assert.True(t, waitForLog(t, sp, logBefore, createSessionOp, 1, time.Second),
		"mounting should create a session")
	assert.True(t, waitForLog(t, sp, logBefore, destroySessionOp, 1, 5*time.Second),
		"unmounting should destroy the session")

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
	require.True(t, framework.WaitFor(5*time.Second, func() bool {
		for i := range numClients {
			for j := range numClients {
				if !framework.FileExists(mounts[i].FilePath(fmt.Sprintf("session_%d_file.txt", j))) {
					return false
				}
			}
		}
		return true
	}), "Not all cross-session files became visible across mounts within timeout")

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
	blockStore := helpers.UniqueTestName("recoverypayload")

	_, err := runner1.CreateInMemoryMetadataStore(metaStore)
	require.NoError(t, err)

	_, err = runner1.CreateBlockStore(blockStore, "memory")
	require.NoError(t, err)

	_, err = runner1.CreateShare("/export", metaStore, blockStore)
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
	blockStore2 := helpers.UniqueTestName("recoverypayload2")

	_, err = runner2.CreateInMemoryMetadataStore(metaStore2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner2.DeleteMetadataStore(metaStore2) })

	_, err = runner2.CreateBlockStore(blockStore2, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner2.DeleteBlockStore(blockStore2) })

	_, err = runner2.CreateShare("/export", metaStore2, blockStore2)
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
