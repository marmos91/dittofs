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
// Each test arranges the two conditions a delegation case needs:
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
// always answers CB_RECALL. The revocation timer itself is covered by
// TestRecallTimer_FiresRevocation in internal/adapter/nfs/v4/state, which arms
// it directly rather than through a sent recall.

var (
	cbNullSucceeded = regexp.MustCompile(`CB_NULL succeeded.*\bclient_id=(\S+)`)
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

	// The server must answer the conflicting OPEN with NFS4ERR_DELAY, recall
	// the delegation, and let the OPEN through only after the holder returns
	// it. Only return-before-OPEN is ordered: the recall goes out from its own
	// goroutine and is logged once the client replies, so its line can land
	// before or after the NFS4ERR_DELAY and DELEGRETURN lines.
	recallLogs := extractNewLogs(recallSince, readLogFile(t, sp))
	delayAt := firstIndex(recallLogs, regexp.MustCompile(`delegation conflict, returning NFS4ERR_DELAY file=`+regexp.QuoteMeta(fileName)))
	recallAt := firstIndex(recallLogs, cbRecallSent)
	returnAt := firstIndex(recallLogs, delegReturned)
	openAt := firstIndex(recallLogs, regexp.MustCompile(`NFSv4 OPEN successful file=`+regexp.QuoteMeta(fileName)))
	require.GreaterOrEqual(t, delayAt, 0, "the conflicting OPEN should first get NFS4ERR_DELAY")
	require.GreaterOrEqual(t, recallAt, 0, "a conflicting write open should recall the holder's delegation")
	require.GreaterOrEqual(t, returnAt, 0, "the holder should return the delegation")
	require.GreaterOrEqual(t, openAt, 0, "the conflicting OPEN should eventually succeed")
	assert.Less(t, returnAt, openAt, "the conflicting OPEN should succeed only after the delegation is returned")

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

// firstIndex returns the offset of re's first match in logs, or -1.
func firstIndex(logs string, re *regexp.Regexp) int {
	if loc := re.FindStringIndex(logs); loc != nil {
		return loc[0]
	}
	return -1
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

// requireCallbackPathsUp waits until n distinct clients have passed a CB_NULL
// probe logged after since, and fails the test, naming the cause, when a probe
// fails or too few complete. It counts client IDs rather than lines because a
// v4.1 client re-probes when its callback parameters change. With the v4.1 backchannel riding the fore channel and v4.0 mounted
// through a non-loopback address, nothing on the host stops the probe, so a
// failure is the server's.
func requireCallbackPathsUp(t *testing.T, sp *helpers.ServerProcess, since string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		logs := extractNewLogs(since, readLogFile(t, sp))
		clients := map[string]bool{}
		for _, m := range cbNullSucceeded.FindAllStringSubmatch(logs, -1) {
			clients[m[1]] = true
		}
		if len(clients) >= n {
			return
		}
		if time.Now().After(deadline) {
			if line := cbNullFailed.FindString(logs); line != "" {
				t.Fatalf("server cannot reach the client's callback path, so it grants no delegations: %s", strings.TrimSpace(line))
			}
			t.Fatalf("%d of %d clients passed a CB_NULL probe of their callback path within 10s; the server grants no delegations without one", len(clients), n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
