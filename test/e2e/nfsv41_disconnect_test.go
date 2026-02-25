//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4.1 Disconnect/Robustness E2E Tests
// =============================================================================
//
// These tests validate that the DittoFS server handles client disconnects
// gracefully, without panics, goroutine leaks, or resource exhaustion.
//
// Each test forces an abrupt client disconnect during an active NFS operation,
// then verifies that:
// 1. The server does not panic or crash
// 2. The server can accept new client connections
// 3. New clients can perform operations successfully
// 4. Server logs show clean connection handling (no goroutine leaks)
//
// All tests use memory/memory stores for fast setup and teardown.

// =============================================================================
// Test 1: Disconnect During Large Write
// =============================================================================

// TestNFSv41DisconnectDuringLargeWrite force-closes a v4.1 client connection
// while a large file write is in progress. The server should handle the
// abrupt disconnection gracefully and remain operational for new clients.
func TestNFSv41DisconnectDuringLargeWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping disconnect during large write test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	sp, _, nfsPort := setupNFSv4TestServer(t)

	// Mount v4.1
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(func() { _ = exec.Command("umount", "-f", mount.Path).Run() })

	// Capture log position before disconnect
	logBefore := readLogFile(t, sp)

	// Start writing a 10MB file in a goroutine
	filePath := mount.FilePath("disconnect_write_test.bin")
	writeStarted := make(chan struct{})
	writeDone := make(chan error, 1)

	go func() {
		data := make([]byte, 10*1024*1024) // 10MB
		for i := range data {
			data[i] = byte(i % 256)
		}

		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			writeDone <- err
			return
		}

		close(writeStarted)

		_, err = f.Write(data)
		if err != nil {
			_ = f.Close()
			writeDone <- err
			return
		}

		err = f.Sync()
		_ = f.Close()
		writeDone <- err
	}()

	// Wait for write to start
	select {
	case <-writeStarted:
		t.Log("Write started, forcing disconnect...")
	case err := <-writeDone:
		// Write completed before we could disconnect
		t.Logf("Write completed before disconnect attempt: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for write to start")
	}

	// Brief delay to let some data flow
	time.Sleep(50 * time.Millisecond)

	// Force unmount (abrupt disconnect)
	forceUnmount(t, mount.Path)

	// Wait for write goroutine to finish (it should error due to unmount)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Logf("Write terminated with error (expected after force unmount): %v", err)
		} else {
			t.Log("Write completed despite force unmount (data may have been fully sent)")
		}
	case <-time.After(15 * time.Second):
		t.Log("Write goroutine timed out (NFS client may be retrying)")
	}

	// Wait for server to clean up the disconnected session
	time.Sleep(2 * time.Second)

	// Verify server is still operational by mounting a new client
	mount2 := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount2.Cleanup)

	// Perform basic operations on new mount
	testFile := mount2.FilePath("post_disconnect_write.txt")
	framework.WriteFile(t, testFile, []byte("Server survived disconnect during write"))
	readContent := framework.ReadFile(t, testFile)
	assert.Equal(t, []byte("Server survived disconnect during write"), readContent,
		"New client should be fully functional after disconnect")

	// Clean up
	_ = os.Remove(testFile)

	// Check server logs for clean handling
	logAfter := readLogFile(t, sp)
	newLogs := extractNewLogs(logBefore, logAfter)

	checkServerLogs(t, newLogs)

	t.Log("TestNFSv41DisconnectDuringLargeWrite: PASSED")
}

// =============================================================================
// Test 2: Disconnect During Directory Listing
// =============================================================================

// TestNFSv41DisconnectDuringReadDir force-closes a v4.1 client connection
// during a directory listing of a directory containing many files. The server
// should handle the disconnect gracefully.
func TestNFSv41DisconnectDuringReadDir(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping disconnect during readdir test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	sp, _, nfsPort := setupNFSv4TestServer(t)

	// First mount: create many files to make READDIR take longer
	setupMount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(func() { _ = exec.Command("umount", "-f", setupMount.Path).Run() })

	dirName := helpers.UniqueTestName("disconnect_readdir")
	dirPath := setupMount.FilePath(dirName)
	framework.CreateDir(t, dirPath)

	const fileCount = 150
	for i := range fileCount {
		filePath := filepath.Join(dirPath, fmt.Sprintf("file_%04d.txt", i))
		framework.WriteFile(t, filePath, []byte(fmt.Sprintf("content for file %d with some padding data", i)))
	}

	setupMount.Cleanup()

	// Second mount: will be disconnected during readdir
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(func() { _ = exec.Command("umount", "-f", mount.Path).Run() })

	// Capture log position
	logBefore := readLogFile(t, sp)

	// Start listing the directory in a goroutine
	readdirMount := mount.FilePath(dirName)
	readdirDone := make(chan error, 1)

	go func() {
		_, err := os.ReadDir(readdirMount)
		readdirDone <- err
	}()

	// Brief delay then force unmount
	time.Sleep(20 * time.Millisecond)
	forceUnmount(t, mount.Path)

	// Wait for readdir to complete or timeout
	select {
	case err := <-readdirDone:
		if err != nil {
			t.Logf("ReadDir terminated with error (expected after force unmount): %v", err)
		} else {
			t.Log("ReadDir completed despite force unmount")
		}
	case <-time.After(15 * time.Second):
		t.Log("ReadDir goroutine timed out")
	}

	// Wait for server cleanup
	time.Sleep(2 * time.Second)

	// Verify server still works
	mount3 := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount3.Cleanup)

	// Verify the files are still there
	dirPath3 := mount3.FilePath(dirName)
	entries, err := os.ReadDir(dirPath3)
	require.NoError(t, err, "New client should be able to read directory")
	assert.Equal(t, fileCount, len(entries),
		"All %d files should still be present after disconnect", fileCount)

	// Perform a write to confirm full functionality
	testFile := mount3.FilePath("post_disconnect_readdir.txt")
	framework.WriteFile(t, testFile, []byte("Server survived disconnect during readdir"))
	readContent := framework.ReadFile(t, testFile)
	assert.Equal(t, []byte("Server survived disconnect during readdir"), readContent)

	// Clean up
	_ = os.Remove(testFile)
	for i := range fileCount {
		_ = os.Remove(filepath.Join(dirPath3, fmt.Sprintf("file_%04d.txt", i)))
	}
	_ = os.Remove(dirPath3)

	// Check server logs
	logAfter := readLogFile(t, sp)
	newLogs := extractNewLogs(logBefore, logAfter)
	checkServerLogs(t, newLogs)

	t.Log("TestNFSv41DisconnectDuringReadDir: PASSED")
}

// =============================================================================
// Test 3: Disconnect During Session Setup
// =============================================================================

// TestNFSv41DisconnectDuringSessionSetup force-kills a mount command mid-way
// through session establishment (EXCHANGE_ID + CREATE_SESSION). The server
// should handle the incomplete session gracefully, and a subsequent full
// mount should succeed.
func TestNFSv41DisconnectDuringSessionSetup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping disconnect during session setup test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	sp, _, nfsPort := setupNFSv4TestServer(t)

	// Capture log position
	logBefore := readLogFile(t, sp)

	// Attempt multiple interrupted mounts to stress session cleanup
	const interruptAttempts = 3
	for i := range interruptAttempts {
		t.Logf("Interrupted mount attempt %d/%d", i+1, interruptAttempts)

		// Create temp mount point
		mountPath, err := os.MkdirTemp("", "dittofs-e2e-disconnect-session-*")
		require.NoError(t, err)

		// Start mount command with a very short context timeout to kill it mid-setup
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)

		mountOpts := fmt.Sprintf("vers=4.1,port=%d,actimeo=0", nfsPort)
		cmd := exec.CommandContext(ctx, "mount", "-t", "nfs",
			"-o", mountOpts,
			"localhost:/export", mountPath)

		output, err := cmd.CombinedOutput()
		cancel()

		if err != nil {
			t.Logf("Interrupted mount (expected): %v output=%s", err, string(output))
		} else {
			// Mount succeeded despite short timeout -- unmount it
			t.Log("Mount completed before timeout -- unmounting")
			forceUnmount(t, mountPath)
		}

		// Clean up mount directory
		_ = os.RemoveAll(mountPath)

		// Brief pause between attempts
		time.Sleep(500 * time.Millisecond)
	}

	// Wait for server to clean up orphaned sessions
	time.Sleep(3 * time.Second)

	// Verify a proper mount succeeds after the interrupted attempts
	mount := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mount.Cleanup)

	// Verify full functionality
	testFile := mount.FilePath("post_interrupted_session.txt")
	framework.WriteFile(t, testFile, []byte("Server survived interrupted session setup"))
	readContent := framework.ReadFile(t, testFile)
	assert.Equal(t, []byte("Server survived interrupted session setup"), readContent,
		"Full mount should work after interrupted session attempts")

	// Create and list directory to test READDIR
	testDir := mount.FilePath("post_interrupt_dir")
	framework.CreateDir(t, testDir)
	for i := range 3 {
		framework.WriteFile(t,
			filepath.Join(testDir, fmt.Sprintf("file_%d.txt", i)),
			[]byte(fmt.Sprintf("content %d", i)))
	}
	entries := framework.ListDir(t, testDir)
	assert.Len(t, entries, 3, "Should list 3 files in directory")

	// Clean up
	_ = os.Remove(testFile)
	for i := range 3 {
		_ = os.Remove(filepath.Join(testDir, fmt.Sprintf("file_%d.txt", i)))
	}
	_ = os.Remove(testDir)

	// Check server logs
	logAfter := readLogFile(t, sp)
	newLogs := extractNewLogs(logBefore, logAfter)
	checkServerLogs(t, newLogs)

	// Check for session reaper activity (cleaning orphaned sessions)
	if strings.Contains(newLogs, "reaper") || strings.Contains(newLogs, "expired") ||
		strings.Contains(newLogs, "cleanup") || strings.Contains(newLogs, "orphan") {
		t.Log("Session reaper activity detected (cleaning up interrupted sessions)")
	} else {
		t.Log("NOTE: No explicit session reaper activity in logs. " +
			"Orphaned sessions may be cleaned up lazily on next lease check.")
	}

	t.Log("TestNFSv41DisconnectDuringSessionSetup: PASSED")
}

// =============================================================================
// Helpers
// =============================================================================

// forceUnmount performs a forced unmount of the given mount path.
func forceUnmount(t *testing.T, mountPath string) {
	t.Helper()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("diskutil", "unmount", "force", mountPath)
	default:
		cmd = exec.Command("umount", "-f", mountPath)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("Force unmount of %s: %v (output: %s)", mountPath, err, string(output))

		// Try lazy unmount as fallback on Linux
		lazyCmd := exec.Command("umount", "-l", mountPath)
		if lazyOutput, lazyErr := lazyCmd.CombinedOutput(); lazyErr != nil {
			t.Logf("Lazy unmount also failed: %v (output: %s)", lazyErr, string(lazyOutput))
		}
	} else {
		t.Logf("Force unmounted %s", mountPath)
	}

	// Clean up mount directory
	_ = os.RemoveAll(mountPath)
}

// checkServerLogs checks server logs for panic or goroutine leak indicators.
func checkServerLogs(t *testing.T, logs string) {
	t.Helper()

	lowLogs := strings.ToLower(logs)

	// Check for panics
	if strings.Contains(lowLogs, "panic:") || strings.Contains(lowLogs, "goroutine ") {
		// Only flag if it looks like an actual panic stack trace
		if strings.Contains(lowLogs, "panic:") && strings.Contains(lowLogs, "goroutine ") {
			t.Error("CRITICAL: Server panic detected in logs after disconnect!")
		}
	}

	// Check for goroutine leak indicators
	if strings.Contains(lowLogs, "goroutine leak") {
		t.Error("Goroutine leak detected in server logs after disconnect!")
	}

	// Check for clean connection handling
	if strings.Contains(logs, "connection closed") || strings.Contains(logs, "Connection closed") ||
		strings.Contains(logs, "client disconnected") || strings.Contains(logs, "EOF") {
		t.Log("Server detected and handled client disconnect cleanly")
	}
}
