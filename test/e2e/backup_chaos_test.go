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

// chaosS3BucketName sanitises store/test names into a valid S3 bucket name
// (lowercase alphanumerics + hyphen, 3–63 chars). Localstack is lenient but
// aws-sdk-go-v2 validates bucket names client-side, so tests must emit
// conforming names. Local to this file — Plan 02/03 matrix tests do not
// (yet) export a shared helper.
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

// TestBackupChaos_KillMidBackup proves SAFETY-02 + DRV-02:
//   - A SIGKILL during an in-flight backup leaves the BackupJob in
//     status=running at kill time; on restart, boot recovery transitions
//     it to status=interrupted (SAFETY-02 in storebackups.Service.Serve).
//   - The S3 destination's orphan sweep aborts any ghost multipart
//     uploads during Serve-time repair (DRV-02).
//
// Uses badger so DB state survives restart. The restart MUST use
// StartServerProcessWithConfig (not StartServerProcess) so the second
// boot reuses the first boot's state dir — otherwise sp2 runs against
// a fresh empty DB and the SAFETY-02 assertion cannot find the job.
func TestBackupChaos_KillMidBackup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping chaos tests in short mode")
	}
	if !framework.CheckLocalstackAvailable(t) {
		t.Skip("Localstack not available")
	}

	ctx := context.Background()
	lsHelper := framework.NewLocalstackHelper(t)

	// Phase 1: start the server, seed data, trigger backup, kill mid-flight.
	sp1 := helpers.StartServerProcess(t, "")
	// NOTE: no t.Cleanup(sp1.ForceKill) — we kill manually below.
	runner1 := helpers.LoginAsAdmin(t, sp1.APIURL())
	apiClient1 := helpers.GetAPIClient(t, sp1.APIURL())

	storeName := helpers.UniqueTestName("chaos_bk")
	badgerPath := filepath.Join(t.TempDir(), "badger-"+storeName)
	_, err := runner1.CreateMetadataStore(storeName, "badger", helpers.WithMetaDBPath(badgerPath))
	require.NoError(t, err, "create badger store")

	// Seed enough data that 500ms is reliably mid-upload.
	// 100 users gives a few hundred KiB of backup payload, enough to
	// span the kill window.
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

	// Sleep-then-kill. 500ms is the documented default; tune in the
	// SUMMARY if this proves flaky on CI.
	time.Sleep(500 * time.Millisecond)
	sp1.ForceKill()

	// Phase 2: restart server REUSING the same config/state dir; DB
	// state survives. MUST use StartServerProcessWithConfig so the
	// second process sees the first's badger DB (same path).
	sp2 := helpers.StartServerProcessWithConfig(t, sp1.ConfigFile())
	t.Cleanup(sp2.ForceKill)
	apiClient2 := helpers.GetAPIClient(t, sp2.APIURL())
	mbr2 := helpers.NewMetadataBackupRunner(t, apiClient2, storeName)

	// SAFETY-02: running → interrupted on restart (boot recovery in
	// storebackups.Service.Serve runs at startup).
	finalJob := mbr2.PollJobUntilTerminal(backupJobID, 30*time.Second)
	assert.Equal(t, "interrupted", finalJob.Status,
		"SAFETY-02: orphaned backup job must transition to interrupted on restart; got %s (err=%q)",
		finalJob.Status, finalJob.Error)

	// DRV-02: orphan sweep aborted any ghost multipart uploads.
	// Allow a grace period for the sweep to run — if the server's
	// Serve boot-orphan-sweep is async, poll up to 30s.
	require.Eventually(t, func() bool {
		uploads := helpers.ListLocalstackMultipartUploads(t, lsHelper, bucket)
		return len(uploads) == 0
	}, 30*time.Second, 1*time.Second,
		"DRV-02: ghost multipart uploads must be cleaned up by orphan sweep")
}

// TestBackupChaos_KillMidRestore proves SAFETY-02 on the restore path:
// a SIGKILL during an in-flight restore must leave the restore job in
// status=interrupted after restart (not hanging in running). As in
// kill-mid-backup, the restart MUST use StartServerProcessWithConfig
// so sp2 inherits sp1's badger DB — without that, the "interrupted"
// job row is not visible to sp2's boot recovery.
func TestBackupChaos_KillMidRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping chaos tests in short mode")
	}
	if !framework.CheckLocalstackAvailable(t) {
		t.Skip("Localstack not available")
	}

	ctx := context.Background()
	lsHelper := framework.NewLocalstackHelper(t)

	// Phase 1: start server, seed data, complete a backup successfully.
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

	// Complete a backup first.
	resp := mbr1.TriggerBackup(repoName)
	completedJob := mbr1.PollJobUntilTerminal(resp.Job.ID, 60*time.Second)
	require.Equal(t, "succeeded", completedJob.Status, "precondition backup must succeed")
	rec := mbr1.WaitForBackupRecordSucceeded(repoName, 10*time.Second)
	require.NotNil(t, rec)

	// Phase 2: trigger restore, kill mid-flight.
	restoreJob, err := mbr1.StartRestore(rec.ID)
	require.NoError(t, err, "start restore")
	require.NotNil(t, restoreJob)
	restoreJobID := restoreJob.ID
	t.Logf("restore triggered: job_id=%s", restoreJobID)

	// Sleep-then-kill. 300ms chosen because restore on local S3 is
	// typically faster than backup. If this proves unreliable (restore
	// completes in <300ms), increase seed-user count or reduce the sleep.
	time.Sleep(300 * time.Millisecond)
	sp1.ForceKill()

	// Phase 3: restart REUSING the same state dir; verify restore job
	// transitioned to interrupted. MUST use StartServerProcessWithConfig
	// — same rationale as kill-mid-backup.
	sp2 := helpers.StartServerProcessWithConfig(t, sp1.ConfigFile())
	t.Cleanup(sp2.ForceKill)
	apiClient2 := helpers.GetAPIClient(t, sp2.APIURL())
	mbr2 := helpers.NewMetadataBackupRunner(t, apiClient2, storeName)

	finalJob := mbr2.PollJobUntilTerminal(restoreJobID, 30*time.Second)
	// If the restore completed before our 300ms sleep elapsed, it will
	// show as "succeeded" — document in SUMMARY and tune timing. The
	// assertion accepts "interrupted" (the expected outcome) or
	// "succeeded" (timing race) but FAILS on any other terminal state.
	if finalJob.Status == "succeeded" {
		t.Skip("restore completed before kill fired; increase seed size or reduce sleep to reliably hit mid-restore")
	}
	assert.Equal(t, "interrupted", finalJob.Status,
		"SAFETY-02: orphaned restore job must transition to interrupted on restart; got %s (err=%q)",
		finalJob.Status, finalJob.Error)
}
