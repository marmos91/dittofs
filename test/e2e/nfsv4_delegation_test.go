//go:build e2e

package e2e

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4 Delegation E2E Tests
// =============================================================================
//
// These tests drive delegation grant and recall through real kernel NFS
// mounts and assert on the server log lines that record each event, since no
// delegation metric is exported.
//
// Two conditions decide whether the case can happen at all, and each test
// skips, naming the condition, when it does not hold:
//
//   - The server grants a delegation only once a CB_NULL probe of the client's
//     callback path has succeeded. A v4.0 client's callback address is the local
//     address of its connection, and the server refuses to dial loopback, so
//     v4.0 mounts go through a non-loopback address of this host.
//   - A recall needs a second NFS client. Two mounts of one export with the same
//     NFS version share a single kernel client (and superblock), so their opens
//     never conflict. A v4.0 and a v4.1 mount are distinct clients with distinct
//     client owners, so the recall tests pair one of each.
//
// Revocation after an unanswered recall is not covered here: the kernel client
// always answers CB_RECALL. TestRecallTimer_FiresRevocation covers it in
// internal/adapter/nfs/v4/state.

var (
	cbNullSucceeded = regexp.MustCompile(`CB_NULL succeeded`)
	cbNullFailed    = regexp.MustCompile(`CB_NULL failed[^\n]*`)
	delegGranted    = regexp.MustCompile(`Delegation granted`)
	cbRecallSent    = regexp.MustCompile(`CB_RECALL (\(v4\.1\) )?sent successfully`)
	cbRecallAny     = regexp.MustCompile(`CB_RECALL`)
	delegReturned   = regexp.MustCompile(`Delegation returned`)
	delegationTrace = regexp.MustCompile(`op_name=(OPEN|WRITE|COMMIT|CLOSE|DELEGRETURN|READ)\b|[Dd]elegation|CB_RECALL`)
)

// =============================================================================
// Test 1: Basic Delegation Lifecycle
// =============================================================================

// TestNFSv4DelegationBasicLifecycle checks that a single client opening a file
// for write is granted a delegation, and that data written under it persists
// through close and reopen.
func TestNFSv4DelegationBasicLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4 delegation basic lifecycle test in short mode")
	}

	framework.SkipIfNFSv4Unsupported(t)

	sp, _, nfsPort := setupNFSv4TestServer(t)
	since := readLogFile(t, sp)

	mount := mountForDelegations(t, nfsPort, "4.0")
	t.Cleanup(mount.Cleanup)
	requireCallbackPathsUp(t, sp, since, 1)

	testData := []byte("Delegation lifecycle test data -- single client exclusive access")
	filePath := mount.FilePath(helpers.UniqueTestName("deleg_basic") + ".txt")

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
	require.NoError(t, err, "Should create file via NFSv4 OPEN")
	t.Cleanup(func() { _ = os.Remove(filePath) })

	require.True(t, waitForLog(t, sp, since, delegGranted, 1, 5*time.Second),
		"server should grant a delegation to the only client with the file open")

	_, err = f.Write(testData)
	require.NoError(t, err, "Should write data under the delegation")
	require.NoError(t, f.Close(), "Should close file")

	readBack, err := os.ReadFile(filePath)
	require.NoError(t, err, "Should read file after close/reopen")
	assert.Equal(t, testData, readBack, "Data should persist through the delegation lifecycle")
}

// =============================================================================
// Test 2: Delegation Recall (v4.0 holder)
// =============================================================================

// TestNFSv4DelegationRecall checks that a write open from a second client
// recalls the delegation a v4.0 client holds over the v4.0 callback
// connection, that the open waits for the delegation to be returned, and that
// the second client then sees the holder's data.
func TestNFSv4DelegationRecall(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4 delegation recall test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)
	testDelegationRecall(t, "4.0", "4.1")
}

// =============================================================================
// Test 3: NFSv4.1 Backchannel Delegation Recall
// =============================================================================

// TestNFSv41BackchannelDelegationRecall is the v4.1 counterpart of
// TestNFSv4DelegationRecall: the holder is a v4.1 client, so the recall travels
// over the session backchannel rather than a server-initiated connection.
func TestNFSv41BackchannelDelegationRecall(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.1 backchannel delegation recall test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)
	testDelegationRecall(t, "4.1", "4.0")
}

// testDelegationRecall mounts the holder with holderVers and the conflicting
// client with otherVers, which must differ so the two are distinct clients.
func testDelegationRecall(t *testing.T, holderVers, otherVers string) {
	t.Helper()

	sp, _, nfsPort := setupNFSv4TestServer(t)
	since := readLogFile(t, sp)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server delegation and I/O log since mount:\n%s",
				grepLines(extractNewLogs(since, readLogFile(t, sp)), delegationTrace))
		}
	})

	holder := mountForDelegations(t, nfsPort, holderVers)
	t.Cleanup(holder.Cleanup)
	other := mountForDelegations(t, nfsPort, otherVers)
	t.Cleanup(other.Cleanup)
	requireCallbackPathsUp(t, sp, since, 2)

	fileName := helpers.UniqueTestName("deleg_recall") + ".txt"
	holderPath := holder.FilePath(fileName)

	f, err := os.OpenFile(holderPath, os.O_CREATE|os.O_RDWR, 0644)
	require.NoError(t, err, "holder: should create file")
	t.Cleanup(func() { _ = os.Remove(holderPath) })
	defer f.Close()

	require.True(t, waitForLog(t, sp, since, delegGranted, 1, 5*time.Second),
		"holder should be granted a delegation")

	// fsync puts the data on the server without giving up the delegation. The
	// recall is not relied on to flush it: the kernel client returns a write
	// delegation and keeps its dirty pages cached under the open stateid.
	testData := []byte("written and fsynced by the delegation holder")
	_, err = f.Write(testData)
	require.NoError(t, err, "holder: should write data")
	require.NoError(t, f.Sync(), "holder: should fsync")

	recallSince := readLogFile(t, sp)
	g, err := os.OpenFile(other.FilePath(fileName), os.O_RDWR, 0)
	require.NoError(t, err, "other client: write open should succeed once the delegation is returned")
	defer g.Close()

	// The server answers the conflicting OPEN with NFS4ERR_DELAY until the
	// holder returns the delegation, so both have happened by the time the
	// open succeeds.
	recallLogs := extractNewLogs(recallSince, readLogFile(t, sp))
	assert.Regexp(t, cbRecallSent, recallLogs, "a conflicting write open should recall the holder's delegation")
	assert.Regexp(t, delegReturned, recallLogs, "the holder should return the delegation before the open succeeds")

	got, err := os.ReadFile(other.FilePath(fileName))
	require.NoError(t, err, "other client: should read file")
	assert.Equal(t, testData, got, "other client should see the holder's data")
}

// =============================================================================
// Test 4: No Delegation Conflict (Concurrent Reads)
// =============================================================================

// TestNFSv4NoDelegationConflict checks that a read delegation is granted to a
// reading client and that a second client reading the same file does not
// recall it.
func TestNFSv4NoDelegationConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4 no-delegation-conflict test in short mode")
	}

	framework.SkipIfNFSv41Unsupported(t)

	sp, _, nfsPort := setupNFSv4TestServer(t)
	since := readLogFile(t, sp)

	// The file is written over NFSv3, which takes no delegation, so no recall
	// precedes the reads and the file is not barred from a new grant.
	writer := framework.MountNFS(t, nfsPort)
	t.Cleanup(writer.Cleanup)
	mount1 := mountForDelegations(t, nfsPort, "4.0")
	t.Cleanup(mount1.Cleanup)
	mount2 := mountForDelegations(t, nfsPort, "4.1")
	t.Cleanup(mount2.Cleanup)
	requireCallbackPathsUp(t, sp, since, 2)

	fileName := helpers.UniqueTestName("deleg_noconflict") + ".txt"
	testData := []byte("Concurrent read delegation test data -- no conflict expected")
	framework.WriteFile(t, writer.FilePath(fileName), testData)
	t.Cleanup(func() { _ = os.Remove(writer.FilePath(fileName)) })

	readSince := readLogFile(t, sp)

	f1, err := os.Open(mount1.FilePath(fileName))
	require.NoError(t, err, "mount1: should open for read")
	defer f1.Close()
	require.True(t, waitForLog(t, sp, readSince, delegGranted, 1, 5*time.Second),
		"the first reader should be granted a read delegation")

	f2, err := os.Open(mount2.FilePath(fileName))
	require.NoError(t, err, "mount2: should open for read")
	defer f2.Close()

	for i, f := range []*os.File{f1, f2} {
		got := make([]byte, len(testData))
		_, err := f.ReadAt(got, 0)
		require.NoError(t, err, "mount%d: should read", i+1)
		assert.Equal(t, testData, got, "mount%d should read correct data", i+1)
	}

	time.Sleep(time.Second)
	assert.NotRegexp(t, cbRecallAny, extractNewLogs(readSince, readLogFile(t, sp)),
		"a second reader should not recall the first reader's delegation")
}

// =============================================================================
// Helpers
// =============================================================================

// readLogFile reads the entire server log file and returns it as a string.
func readLogFile(t *testing.T, sp *helpers.ServerProcess) string {
	t.Helper()

	content, err := os.ReadFile(sp.LogFile())
	if err != nil {
		// Log file may not exist yet
		t.Logf("Could not read log file: %v", err)
		return ""
	}
	return string(content)
}

// extractNewLogs returns the portion of logAfter that is new compared to logBefore.
// This allows detecting log lines produced during a specific test operation.
func extractNewLogs(logBefore, logAfter string) string {
	if len(logAfter) > len(logBefore) {
		return logAfter[len(logBefore):]
	}
	return ""
}

// mountForDelegations mounts /export at vers, through a non-loopback address
// for v4.0 so the server can dial its callback path.
func mountForDelegations(t *testing.T, port int, vers string) *framework.Mount {
	t.Helper()
	if vers == "4.0" {
		return framework.MountNFSWithVersionAt(t, framework.NonLoopbackIPv4(t), port, vers)
	}
	return framework.MountNFSWithVersion(t, port, vers)
}

// grepLines returns the lines of logs that re matches.
func grepLines(logs string, re *regexp.Regexp) string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if re.MatchString(line) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// waitForLog polls the server log until re matches at least n times in what
// was written after since, and reports whether it did within timeout.
func waitForLog(t *testing.T, sp *helpers.ServerProcess, since string, re *regexp.Regexp, n int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if len(re.FindAllStringIndex(extractNewLogs(since, readLogFile(t, sp)), -1)) >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireCallbackPathsUp waits for n successful CB_NULL probes logged after
// since and skips the test, naming the cause, when a probe fails or none
// completes: without a verified callback path the server grants no
// delegations, so there is nothing for the caller to observe.
func requireCallbackPathsUp(t *testing.T, sp *helpers.ServerProcess, since string, n int) {
	t.Helper()
	if waitForLog(t, sp, since, cbNullSucceeded, n, 10*time.Second) {
		return
	}
	if line := cbNullFailed.FindString(extractNewLogs(since, readLogFile(t, sp))); line != "" {
		t.Skipf("server cannot reach the client's callback path, so it grants no delegations: %s", strings.TrimSpace(line))
	}
	t.Skipf("no CB_NULL probe of the client's callback path succeeded within 10s; the server grants no delegations without one")
}
