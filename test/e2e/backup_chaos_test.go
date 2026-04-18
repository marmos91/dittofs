//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
)

// chaosS3BucketName sanitises names into S3-conforming bucket names.
// aws-sdk-go-v2 validates bucket names client-side even against Localstack.
func chaosS3BucketName(raw string) string {
	lower := strings.ToLower(raw)
	clean := regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(lower, "-")
	clean = strings.Trim(clean, "-")
	if len(clean) < 3 {
		clean = clean + "-buk"
	}
	if len(clean) > 63 {
		clean = clean[:63]
		clean = strings.TrimRight(clean, "-")
	}
	return clean
}

// TestBackupChaos_KillMidBackup verifies that an orphaned backup job transitions
// to interrupted on restart and that ghost multipart uploads are cleaned up.
// Uses badger so DB state survives the kill. Must use StartServerProcessWithConfig
// on restart so sp2 sees sp1's badger DB — a fresh DB would hide the job row.
func TestBackupChaos_KillMidBackup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping chaos tests in short mode")
	}
	if !framework.CheckLocalstackAvailable(t) {
		t.Skip("Localstack not available")
	}

	ctx := context.Background()
	lsHelper := framework.NewLocalstackHelper(t)

	sp1 := helpers.StartServerProcess(t, "")
	runner1 := helpers.LoginAsAdmin(t, sp1.APIURL())
	apiClient1 := helpers.GetAPIClient(t, sp1.APIURL())

	storeName := helpers.UniqueTestName("chaos_bk")
	badgerPath := filepath.Join(t.TempDir(), "badger-"+storeName)
	_, err := runner1.CreateMetadataStore(storeName, "badger", helpers.WithMetaDBPath(badgerPath))
	require.NoError(t, err, "create badger store")

	// 100 users gives a few hundred KiB of backup payload — enough to span the kill window.
	for i := 0; i < 100; i++ {
		_, err := runner1.CreateUser(
			helpers.UniqueTestName(fmt.Sprintf("chaos_u_%d", i)),
			"testpass123",
			helpers.WithEmail(fmt.Sprintf("chaos%d@test.com", i)),
		)
		require.NoError(t, err, "seed user %d", i)
	}

	bucket := chaosS3BucketName("chaos-bk-" + storeName)
	require.NoError(t, lsHelper.CreateBucket(ctx, bucket))
	t.Cleanup(func() { lsHelper.CleanupBucket(ctx, bucket) })

	repoName := helpers.UniqueTestName("chaos_repo")
	mbr1 := helpers.NewMetadataBackupRunner(t, apiClient1, storeName)
	_ = mbr1.CreateS3Repo(repoName, bucket, lsHelper.Endpoint)

	resp := mbr1.TriggerBackup(repoName)
	backupJobID := resp.Job.ID
	t.Logf("backup triggered: job_id=%s", backupJobID)

	time.Sleep(500 * time.Millisecond)
	sp1.ForceKill()

	sp2 := helpers.StartServerProcessWithConfig(t, sp1.ConfigFile())
	t.Cleanup(sp2.ForceKill)
	apiClient2 := helpers.GetAPIClient(t, sp2.APIURL())
	mbr2 := helpers.NewMetadataBackupRunner(t, apiClient2, storeName)

	finalJob := mbr2.PollJobUntilTerminal(backupJobID, 30*time.Second)
	assert.Equal(t, "interrupted", finalJob.Status,
		"orphaned backup job must transition to interrupted on restart; got %s (err=%q)",
		finalJob.Status, finalJob.Error)

	require.Eventually(t, func() bool {
		uploads := helpers.ListLocalstackMultipartUploads(t, lsHelper, bucket)
		return len(uploads) == 0
	}, 30*time.Second, 1*time.Second,
		"ghost multipart uploads must be cleaned up by orphan sweep")
}

// TestBackupChaos_KillMidRestore verifies that an orphaned restore job transitions
// to interrupted on restart. Must use StartServerProcessWithConfig so sp2 inherits
// sp1's badger DB — without it the job row is invisible to boot recovery.
func TestBackupChaos_KillMidRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping chaos tests in short mode")
	}
	if !framework.CheckLocalstackAvailable(t) {
		t.Skip("Localstack not available")
	}

	ctx := context.Background()
	lsHelper := framework.NewLocalstackHelper(t)

	sp1 := helpers.StartServerProcess(t, "")
	t.Cleanup(sp1.ForceKill)
	runner1 := helpers.LoginAsAdmin(t, sp1.APIURL())
	apiClient1 := helpers.GetAPIClient(t, sp1.APIURL())

	storeName := helpers.UniqueTestName("chaos_rs")
	badgerPath := filepath.Join(t.TempDir(), "badger-"+storeName)
	_, err := runner1.CreateMetadataStore(storeName, "badger", helpers.WithMetaDBPath(badgerPath))
	require.NoError(t, err, "create badger store")

	for i := 0; i < 50; i++ {
		_, err := runner1.CreateUser(
			helpers.UniqueTestName(fmt.Sprintf("rs_u_%d", i)),
			"testpass123",
			helpers.WithEmail(fmt.Sprintf("rs%d@test.com", i)),
		)
		require.NoError(t, err, "seed user %d", i)
	}

	bucket := chaosS3BucketName("chaos-rs-" + storeName)
	require.NoError(t, lsHelper.CreateBucket(ctx, bucket))
	t.Cleanup(func() { lsHelper.CleanupBucket(ctx, bucket) })

	repoName := helpers.UniqueTestName("rs_repo")
	mbr1 := helpers.NewMetadataBackupRunner(t, apiClient1, storeName)
	_ = mbr1.CreateS3Repo(repoName, bucket, lsHelper.Endpoint)

	resp := mbr1.TriggerBackup(repoName)
	completedJob := mbr1.PollJobUntilTerminal(resp.Job.ID, 60*time.Second)
	require.Equal(t, "succeeded", completedJob.Status, "precondition backup must succeed")
	rec := mbr1.WaitForBackupRecordSucceeded(repoName, 10*time.Second)
	require.NotNil(t, rec)

	restoreJob, err := mbr1.StartRestore(rec.ID)
	require.NoError(t, err, "start restore")
	require.NotNil(t, restoreJob)
	restoreJobID := restoreJob.ID
	t.Logf("restore triggered: job_id=%s", restoreJobID)

	// 300ms chosen because restore is typically faster than backup on local S3.
	// If this proves unreliable, increase seed-user count.
	time.Sleep(300 * time.Millisecond)
	sp1.ForceKill()

	sp2 := helpers.StartServerProcessWithConfig(t, sp1.ConfigFile())
	t.Cleanup(sp2.ForceKill)
	apiClient2 := helpers.GetAPIClient(t, sp2.APIURL())
	mbr2 := helpers.NewMetadataBackupRunner(t, apiClient2, storeName)

	finalJob := mbr2.PollJobUntilTerminal(restoreJobID, 30*time.Second)
	if finalJob.Status == "succeeded" {
		t.Skip("restore completed before kill fired; increase seed size or reduce sleep to reliably hit mid-restore")
	}
	assert.Equal(t, "interrupted", finalJob.Status,
		"orphaned restore job must transition to interrupted on restart; got %s (err=%q)",
		finalJob.Status, finalJob.Error)
}
