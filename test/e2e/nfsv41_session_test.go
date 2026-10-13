//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	createSessionOp  = regexp.MustCompile(`op_name=CREATE_SESSION status=0\b`)
	destroySessionOp = regexp.MustCompile(`op_name=DESTROY_SESSION status=0\b`)
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

// TestNFSv41IOSurvivesPacketLoss writes and fsyncs the first half of a 10MB
// file, drops the client's packets to the server for 500ms while the second
// half is written and fsynced, and requires the file to read back intact. A DROP stalls TCP rather than breaking the
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

	// Mount over IPv4: "localhost" resolves to ::1, which the iptables rule
	// below does not filter.
	mount := framework.MountNFSWithVersionAt(t, "127.0.0.1", nfsPort, "4.1")
	t.Cleanup(mount.Cleanup)

	filePath := mount.FilePath("packet_loss_test.bin")
	t.Cleanup(func() { _ = os.Remove(filePath) })
	data := make([]byte, 10*1024*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
	require.NoError(t, err, "create file")
	defer f.Close()
	half := len(data) / 2
	_, err = f.Write(data[:half])
	require.NoError(t, err, "write first half")
	require.NoError(t, f.Sync(), "fsync first half")

	// The rule goes in before the second half is written, so its fsync cannot
	// complete until the rule is removed; the packet counter proves traffic hit
	// it.
	comment := fmt.Sprintf("dittofs-e2e-drop-%d", nfsPort)
	ruleArgs := []string{"OUTPUT", "-p", "tcp", "--dport", fmt.Sprintf("%d", nfsPort),
		"-m", "comment", "--comment", comment, "-j", "DROP"}
	out, err := exec.Command("iptables", append([]string{"-A"}, ruleArgs...)...).CombinedOutput()
	require.NoError(t, err, "add iptables DROP rule: %s", string(out))
	removeRule := func() { _ = exec.Command("iptables", append([]string{"-D"}, ruleArgs...)...).Run() }
	t.Cleanup(removeRule)

	writeErr := make(chan error, 1)
	go func() {
		if _, err := f.Write(data[half:]); err != nil {
			writeErr <- fmt.Errorf("write second half: %w", err)
			return
		}
		writeErr <- f.Sync()
	}()

	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-writeErr:
		t.Fatalf("fsync finished while every packet to the server was dropped (err=%v)", err)
	default:
	}
	assert.Positive(t, droppedPackets(t, comment), "the DROP rule should have dropped packets to the server")
	removeRule()

	select {
	case err := <-writeErr:
		require.NoError(t, err, "the second half should write and fsync across the packet loss")
	case <-time.After(60 * time.Second):
		t.Fatal("fsync did not complete within 60s of the packet loss ending")
	}

	got, err := os.ReadFile(filePath)
	require.NoError(t, err, "read back")
	assert.True(t, bytes.Equal(data, got), "file should read back intact (%d bytes, want %d)", len(got), len(data))
}

// droppedPackets returns the packet counter of the OUTPUT rule tagged with
// comment.
func droppedPackets(t *testing.T, comment string) int64 {
	t.Helper()
	out, err := exec.Command("iptables", "-L", "OUTPUT", "-v", "-x", "-n").CombinedOutput()
	require.NoError(t, err, "list iptables OUTPUT: %s", string(out))
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "/* "+comment+" */") {
			continue
		}
		fields := strings.Fields(line)
		require.NotEmpty(t, fields)
		n, err := strconv.ParseInt(fields[0], 10, 64)
		require.NoError(t, err, "parse packet counter in %q", line)
		return n
	}
	t.Fatalf("no iptables OUTPUT rule tagged %q:\n%s", comment, out)
	return 0
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

// TestNFSv41SessionRecoveryAfterRestart restarts the server underneath a live
// v4.1 mount and requires the mount to carry on. The restarted server runs on
// the same state directory and the same badger metadata store, so the mount's
// file handles stay valid and only its session is lost: the client must
// establish a new one, and then read back a file written before the restart and
// create a new one.
func TestNFSv41SessionRecoveryAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 session recovery test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	sp1 := helpers.StartServerProcess(t, "")
	t.Cleanup(sp1.ForceKill)
	runner1 := helpers.LoginAsAdmin(t, sp1.APIURL())

	metaStore := helpers.UniqueTestName("recoverymeta")
	blockStore := helpers.UniqueTestName("recoverypayload")

	_, err := runner1.CreateMetadataStore(metaStore, "badger",
		helpers.WithMetaDBPath(filepath.Join(t.TempDir(), "badger")))
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

	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount.Cleanup)

	before := []byte("before restart")
	testFile := mount.FilePath("recovery_test.txt")
	framework.WriteFile(t, testFile, before)

	require.NoError(t, sp1.StopGracefully(), "first server should shut down cleanly")
	logBefore := readLogFile(t, sp1)

	// The stores, the share and the enabled NFS adapter all come back from the
	// control-plane database in the shared state directory.
	sp2 := helpers.RestartServerProcess(t, sp1)
	t.Cleanup(sp2.ForceKill)
	runner2 := helpers.LoginAsAdmin(t, sp2.APIURL())

	err = helpers.WaitForAdapterStatus(t, runner2, "nfs", true, 10*time.Second)
	require.NoError(t, err)
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// The deadline covers the full default grace period, which holds new opens
	// until it ends; a client that reclaims and sends RECLAIM_COMPLETE ends it
	// early.
	after := []byte("after restart")
	newFile := mount.FilePath("after_restart.txt")
	t.Cleanup(func() { _ = os.Remove(newFile) })

	checkRecovered := func() error {
		got, err := os.ReadFile(testFile)
		if err != nil {
			return fmt.Errorf("read file written before restart: %w", err)
		}
		if !bytes.Equal(got, before) {
			return fmt.Errorf("file written before restart reads back %q, want %q", got, before)
		}
		if err := os.WriteFile(newFile, after, 0644); err != nil {
			return fmt.Errorf("create file after restart: %w", err)
		}
		got, err = os.ReadFile(newFile)
		if err != nil {
			return fmt.Errorf("read file created after restart: %w", err)
		}
		if !bytes.Equal(got, after) {
			return fmt.Errorf("file created after restart reads back %q, want %q", got, after)
		}
		return nil
	}

	deadline := time.Now().Add(120 * time.Second)
	for {
		err = checkRecovered()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			sp2.DumpLogs(t)
			t.Fatalf("mount did not recover after server restart: %v", err)
		}
		time.Sleep(time.Second)
	}

	// Both files are still in the client's page cache, and the change attribute
	// survives in badger, so the reads above may never have reached the server.
	// Drop the cache so these go over the wire and the restarted share's journal
	// has to serve the bytes.
	require.NoError(t, os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0),
		"dropping the page cache needs root")
	got, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, before, got, "file written before restart should read back from the server")
	got, err = os.ReadFile(newFile)
	require.NoError(t, err)
	assert.Equal(t, after, got, "file created after restart should read back from the server")

	assert.True(t, waitForLog(t, sp2, logBefore, createSessionOp, 1, 5*time.Second),
		"the client should have created a new session on the restarted server")
}
