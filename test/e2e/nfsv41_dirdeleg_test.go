//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4.1 Directory Delegation E2E Tests
// =============================================================================
//
// These tests check that a directory delegation is granted and that its holder
// learns of a change made by another client, for each mutation type: entry
// added, removed, renamed, and attribute changed. A holder that asked for that
// notification type must get a CB_NOTIFY; one that did not must have the
// delegation recalled, except for an entry's attribute change: the delegation
// covers the directory's attributes and entries, not its entries' attributes,
// so RFC 8881 lets the server keep it then.
//
// The holder is a v4.1 mount, which reads the directory to provoke a
// GET_DIR_DELEGATION. The mutating client is a v4.0 mount: two v4.1 mounts of
// one export share a kernel client, and a holder is not notified of its own
// changes. When the kernel client sends no GET_DIR_DELEGATION, there is no
// delegation to observe and the test skips saying so.

var (
	getDirDelegationOp        = regexp.MustCompile(`op_name=GET_DIR_DELEGATION`)
	dirDelegGranted           = regexp.MustCompile(`Directory delegation granted[^\n]*notification_mask=0x([0-9a-f]+)`)
	cbNotifyQueued            = regexp.MustCompile(`CB_NOTIFY queued`)
	backchannelCallbackFailed = regexp.MustCompile(`BackchannelSender callback failed`)
	dirDelegReturnedRev       = regexp.MustCompile(`op_name=DELEGRETURN status=0\b|Delegation revoked with the client's state`)
)

// Notification type bit numbers (RFC 8881 §20.4, notify_type4).
const (
	notifyChangeChildAttrs = 0
	notifyRemoveEntry      = 2
	notifyAddEntry         = 3
	notifyRenameEntry      = 4
)

// dirDelegFixture is a holder mount with a directory delegation and a second,
// distinct client that mutates the directory.
type dirDelegFixture struct {
	sp         *helpers.ServerProcess
	notifMask  uint64 // notification types the holder asked for
	holder     *framework.Mount
	holderDir  string
	otherDir   string
	mutateFrom string // log snapshot taken once the delegation is held
}

// setupDirDelegation creates dirName holding files, makes the holder read it,
// and requires the resulting directory delegation, skipping when the kernel
// client does not ask for one.
func setupDirDelegation(t *testing.T, dirName string, files ...string) *dirDelegFixture {
	t.Helper()

	sp, _, nfsPort := setupNFSv4TestServer(t)
	since := readLogFile(t, sp)

	holder := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(holder.Cleanup)
	requireCallbackPathsUp(t, sp, since, 1)

	other := framework.MountNFSWithVersion(t, nfsPort, "4.0")
	t.Cleanup(other.Cleanup)

	// The other client builds the directory, so the holder takes no file
	// delegations of its own and its first access to the directory is the
	// READDIR below.
	otherDir := other.FilePath(dirName)
	framework.CreateDir(t, otherDir)
	t.Cleanup(func() { _ = os.RemoveAll(otherDir) })
	for _, f := range files {
		framework.WriteFile(t, filepath.Join(otherDir, f), []byte("content of "+f))
	}
	holderDir := holder.FilePath(dirName)

	// Snapshot before the READDIR, which is what provokes GET_DIR_DELEGATION.
	readdirSince := readLogFile(t, sp)
	entries := framework.ListDir(t, holderDir)
	require.Len(t, entries, len(files), "holder should list the directory")

	// decision: the server recalls a directory delegation on every entry change
	// instead of notifying, so a test here passes when the client asked for no
	// notification of its change type and fails when it asked for one.
	if !waitForLog(t, sp, readdirSince, getDirDelegationOp, 1, 2*time.Second) {
		t.Skip("the kernel NFS client sent no GET_DIR_DELEGATION, so there is no directory delegation to observe")
	}
	require.True(t, waitForLog(t, sp, readdirSince, dirDelegGranted, 1, 2*time.Second),
		"server should grant the directory delegation the client asked for")
	m := dirDelegGranted.FindStringSubmatch(extractNewLogs(readdirSince, readLogFile(t, sp)))
	mask, err := strconv.ParseUint(m[1], 16, 32)
	require.NoError(t, err, "parse notification mask %q", m[1])

	return &dirDelegFixture{
		sp:         sp,
		notifMask:  mask,
		holder:     holder,
		holderDir:  holderDir,
		otherDir:   otherDir,
		mutateFrom: readLogFile(t, sp),
	}
}

// requireNotified requires the holder to learn of a change of type notifyType
// made by another client: through CB_NOTIFY when it asked for that type, or
// through a recall of the delegation when it did not.
func (f *dirDelegFixture) requireNotified(t *testing.T, what string, notifyType uint) {
	t.Helper()
	if f.notifMask&(1<<notifyType) != 0 {
		require.True(t, waitForLog(t, f.sp, f.mutateFrom, cbNotifyQueued, 1, 5*time.Second),
			"%s by another client should queue a CB_NOTIFY to the delegation holder", what)
		// Queued is not delivered: a CB_NOTIFY the client rejects shows up as a
		// failed backchannel callback.
		assert.False(t, waitForLog(t, f.sp, f.mutateFrom, backchannelCallbackFailed, 1, time.Second),
			"the CB_NOTIFY should reach the holder")
		return
	}
	require.True(t, waitForLog(t, f.sp, f.mutateFrom, cbRecallSent, 1, 5*time.Second),
		"%s by another client should recall a delegation that asked for no such notification", what)
}

// =============================================================================
// Test 1: CB_NOTIFY on File Creation (Entry Added)
// =============================================================================

func TestNFSv41DirDelegationEntryAdded(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping directory delegation entry-added test in short mode")
	}
	framework.SkipIfNFSv41Unsupported(t)

	f := setupDirDelegation(t, helpers.UniqueTestName("dirdeleg_add"))

	framework.WriteFile(t, filepath.Join(f.otherDir, "added_file.txt"), []byte("new file via client 2"))
	f.requireNotified(t, "creating an entry", notifyAddEntry)

	framework.WaitForFile(t, filepath.Join(f.holderDir, "added_file.txt"), 5*time.Second)
	assert.Contains(t, framework.ListDir(t, f.holderDir), "added_file.txt",
		"holder should see the file the other client created")
}

// =============================================================================
// Test 2: CB_NOTIFY on File Removal (Entry Removed)
// =============================================================================

func TestNFSv41DirDelegationEntryRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping directory delegation entry-removed test in short mode")
	}
	framework.SkipIfNFSv41Unsupported(t)

	f := setupDirDelegation(t, helpers.UniqueTestName("dirdeleg_remove"), "to_remove.txt")

	target := filepath.Join(f.otherDir, "to_remove.txt")
	framework.WaitForFile(t, target, 5*time.Second)
	require.NoError(t, os.Remove(target), "other client: should delete file")
	f.requireNotified(t, "removing an entry", notifyRemoveEntry)

	framework.WaitForDirEntryCount(t, f.holderDir, 0, 5*time.Second)
	assert.Empty(t, framework.ListDir(t, f.holderDir), "holder should see the directory empty")
}

// =============================================================================
// Test 3: CB_NOTIFY on File Rename (Entry Renamed)
// =============================================================================

func TestNFSv41DirDelegationEntryRenamed(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping directory delegation entry-renamed test in short mode")
	}
	framework.SkipIfNFSv41Unsupported(t)

	f := setupDirDelegation(t, helpers.UniqueTestName("dirdeleg_rename"), "original_name.txt")

	src := filepath.Join(f.otherDir, "original_name.txt")
	framework.WaitForFile(t, src, 5*time.Second)
	require.NoError(t, os.Rename(src, filepath.Join(f.otherDir, "renamed_file.txt")),
		"other client: should rename file")
	f.requireNotified(t, "renaming an entry", notifyRenameEntry)

	framework.WaitForFile(t, filepath.Join(f.holderDir, "renamed_file.txt"), 5*time.Second)
	entries := framework.ListDir(t, f.holderDir)
	assert.False(t, slices.Contains(entries, "original_name.txt"), "holder should not see the old name")
	assert.True(t, slices.Contains(entries, "renamed_file.txt"), "holder should see the new name")
}

// =============================================================================
// Test 4: CB_NOTIFY on Attribute Change
// =============================================================================

// A mode change is a significant attribute change; atime/ctime changes alone
// do not notify. A holder that did not ask for that notification learns of the
// new mode through its attribute cache, as without a delegation.
func TestNFSv41DirDelegationAttrChanged(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping directory delegation attr-changed test in short mode")
	}
	framework.SkipIfNFSv41Unsupported(t)

	f := setupDirDelegation(t, helpers.UniqueTestName("dirdeleg_attr"), "attr_test.txt")

	target := filepath.Join(f.otherDir, "attr_test.txt")
	framework.WaitForFile(t, target, 5*time.Second)
	require.NoError(t, os.Chmod(target, 0755), "other client: should chmod file")
	if f.notifMask&(1<<notifyChangeChildAttrs) != 0 {
		f.requireNotified(t, "changing an entry's mode", notifyChangeChildAttrs)
	}

	holderFile := filepath.Join(f.holderDir, "attr_test.txt")
	require.True(t, framework.WaitFor(5*time.Second, func() bool {
		fi, err := os.Stat(holderFile)
		return err == nil && fi.Mode().Perm()&0100 != 0
	}), "holder should see the mode change")
}

// =============================================================================
// Test 5: Directory Delegation Cleanup on Unmount
// =============================================================================

// TestNFSv41DirDelegationCleanup checks that unmounting the holder releases
// its directory delegation, and that a new client then sees the directory
// intact.
func TestNFSv41DirDelegationCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping directory delegation cleanup test in short mode")
	}
	framework.SkipIfNFSv41Unsupported(t)

	files := make([]string, 5)
	for i := range files {
		files[i] = fmt.Sprintf("cleanup_%d.txt", i)
	}
	dirName := helpers.UniqueTestName("dirdeleg_cleanup")
	f := setupDirDelegation(t, dirName, files...)

	f.holder.Unmount()

	require.True(t, waitForLog(t, f.sp, f.mutateFrom, dirDelegReturnedRev, 1, 10*time.Second),
		"unmounting the holder should return or revoke its directory delegation")

	got := framework.ListDir(t, f.otherDir)
	assert.ElementsMatch(t, files, got, "another client should see the directory intact")
}
