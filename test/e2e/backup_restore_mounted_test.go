//go:build e2e

package e2e

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/test/e2e/helpers"
)

func setupMountedRestoreFixture(t *testing.T) (*helpers.MetadataBackupRunner, string, string) {
	t.Helper()

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)
	runner := helpers.LoginAsAdmin(t, sp.APIURL())
	apiClient := helpers.GetAPIClient(t, sp.APIURL())

	storeName := helpers.UniqueTestName("mr_meta")
	_, err := runner.CreateMetadataStore(storeName, "memory")
	require.NoError(t, err, "create metadata store")

	localStoreName := helpers.UniqueTestName("mr_local")
	_, err = runner.CreateLocalBlockStore(localStoreName, "memory")
	require.NoError(t, err, "create local block store")

	shareName := "/" + helpers.UniqueTestName("mr_share")
	share, err := runner.CreateShare(shareName, storeName, localStoreName)
	require.NoError(t, err, "create share")
	require.NotNil(t, share, "create share must return share")

	mbr := helpers.NewMetadataBackupRunner(t, apiClient, storeName)
	repoName := helpers.UniqueTestName("mr_repo")
	repoPath := filepath.Join(t.TempDir(), "mr-backups")
	_ = mbr.CreateLocalRepo(repoName, repoPath)

	resp := mbr.TriggerBackup(repoName)
	job := mbr.PollJobUntilTerminal(resp.Job.ID, 60*time.Second)
	require.Equal(t, "succeeded", job.Status, "precondition backup must succeed")
	rec := mbr.WaitForBackupRecordSucceeded(repoName, 10*time.Second)
	require.NotNil(t, rec)

	return mbr, shareName, rec.ID
}

// TestBackupRestoreMounted_Rejected409 asserts that restore returns 409 when any
// share on the target store has Enabled=true.
func TestBackupRestoreMounted_Rejected409(t *testing.T) {
	mbr, shareName, recordID := setupMountedRestoreFixture(t)

	enabledShares := mbr.StartRestoreExpectPrecondition(recordID)
	assert.Contains(t, enabledShares, shareName,
		"enabled_shares must include the share blocking restore; got %v", enabledShares)
}

// TestBackupRestoreMounted_DisabledAcceptsRestore asserts that disabling the
// blocking share clears the 409 precondition and restore succeeds.
func TestBackupRestoreMounted_DisabledAcceptsRestore(t *testing.T) {
	mbr, shareName, recordID := setupMountedRestoreFixture(t)

	share, err := mbr.Client.DisableShare(shareName)
	require.NoError(t, err, "DisableShare")
	require.False(t, share.Enabled, "share must be disabled after DisableShare")

	restoreJob := mbr.StartRestoreMustSucceed(recordID)
	require.NotNil(t, restoreJob)
	final := mbr.PollJobUntilTerminal(restoreJob.ID, 60*time.Second)
	assert.Equal(t, "succeeded", final.Status,
		"restore after DisableShare must succeed; got %s (err=%q)", final.Status, final.Error)
}
