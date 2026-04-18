//go:build e2e

package helpers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/e2e/framework"
)

// MetadataBackupRunner wraps *apiclient.Client with metadata-backup test
// helpers scoped to a single metadata store. Used by Phase-7 E2E and
// chaos tests to avoid re-implementing repo setup / trigger / poll
// boilerplate in every test file.
//
// Each subtest is expected to construct its own MetadataBackupRunner
// bound to a fresh (isolated by unique store name) client context — the
// helper does NOT protect against cross-subtest mutation of a shared
// apiclient.Client (T-07-07).
type MetadataBackupRunner struct {
	T         *testing.T
	Client    *apiclient.Client
	StoreName string
}

// NewMetadataBackupRunner constructs a helper bound to the given metadata store.
func NewMetadataBackupRunner(t *testing.T, client *apiclient.Client, storeName string) *MetadataBackupRunner {
	return &MetadataBackupRunner{T: t, Client: client, StoreName: storeName}
}

// CreateLocalRepo creates a kind="local" BackupRepo pointing at path.
func (r *MetadataBackupRunner) CreateLocalRepo(repoName, path string) *apiclient.BackupRepo {
	r.T.Helper()
	repo, err := r.Client.CreateBackupRepo(r.StoreName, &apiclient.BackupRepoRequest{
		Name: repoName,
		Kind: "local",
		Config: map[string]any{
			"path":         path,
			"grace_window": "24h",
		},
	})
	require.NoError(r.T, err, "CreateBackupRepo(local) failed")
	return repo
}

// CreateS3Repo creates a kind="s3" BackupRepo against a Localstack endpoint.
func (r *MetadataBackupRunner) CreateS3Repo(repoName, bucket, endpoint string) *apiclient.BackupRepo {
	r.T.Helper()
	repo, err := r.Client.CreateBackupRepo(r.StoreName, &apiclient.BackupRepoRequest{
		Name: repoName,
		Kind: "s3",
		Config: map[string]any{
			"bucket":           bucket,
			"region":           "us-east-1",
			"endpoint":         endpoint,
			"access_key":       "test",
			"secret_key":       "test",
			"force_path_style": true,
			"max_retries":      3,
			"grace_window":     "24h",
		},
	})
	require.NoError(r.T, err, "CreateBackupRepo(s3) failed")
	return repo
}

// TriggerBackup invokes POST /backups with the given repo name and
// fails the test on any transport / typed-problem error. Returns the
// full response including the spawned Job (guaranteed non-nil).
func (r *MetadataBackupRunner) TriggerBackup(repoName string) *apiclient.TriggerBackupResponse {
	r.T.Helper()
	resp, err := r.Client.TriggerBackup(r.StoreName, &apiclient.TriggerBackupRequest{Repo: repoName})
	require.NoError(r.T, err, "TriggerBackup failed")
	require.NotNil(r.T, resp, "TriggerBackup must return a non-nil response")
	require.NotNil(r.T, resp.Job, "TriggerBackup must return a Job")
	return resp
}

// PollJobUntilTerminal polls GetBackupJob every 500ms until status is
// one of {succeeded, failed, interrupted, canceled}, then returns the
// final job row. Fails the test if polling exceeds timeout (T-07-08:
// fail-fast rather than spin infinitely).
func (r *MetadataBackupRunner) PollJobUntilTerminal(jobID string, timeout time.Duration) *apiclient.BackupJob {
	r.T.Helper()
	var finalJob *apiclient.BackupJob
	require.Eventually(r.T, func() bool {
		job, err := r.Client.GetBackupJob(r.StoreName, jobID)
		if err != nil {
			return false
		}
		switch job.Status {
		case "succeeded", "failed", "interrupted", "canceled":
			finalJob = job
			return true
		}
		return false
	}, timeout, 500*time.Millisecond, "job %s did not reach terminal state within %s", jobID, timeout)
	return finalJob
}

// StartRestore invokes POST /restore and RETURNS the error so callers
// can assert on *apiclient.RestorePreconditionError via errors.As.
func (r *MetadataBackupRunner) StartRestore(fromBackupID string) (*apiclient.BackupJob, error) {
	r.T.Helper()
	return r.Client.StartRestore(r.StoreName, &apiclient.RestoreRequest{FromBackupID: fromBackupID})
}

// StartRestoreMustSucceed calls StartRestore and fails the test if the
// API returns an error (including *RestorePreconditionError).
func (r *MetadataBackupRunner) StartRestoreMustSucceed(fromBackupID string) *apiclient.BackupJob {
	r.T.Helper()
	job, err := r.StartRestore(fromBackupID)
	require.NoError(r.T, err, "StartRestore must succeed; enable-share precondition already cleared?")
	return job
}

// StartRestoreExpectPrecondition calls StartRestore and asserts the
// returned error is *apiclient.RestorePreconditionError with at least
// one enabled share. Returns the slice of enabled shares for further
// assertions.
func (r *MetadataBackupRunner) StartRestoreExpectPrecondition(fromBackupID string) []string {
	r.T.Helper()
	_, err := r.StartRestore(fromBackupID)
	require.Error(r.T, err, "StartRestore must 409 when shares enabled")
	var preErr *apiclient.RestorePreconditionError
	require.True(r.T, errors.As(err, &preErr), "err must be *RestorePreconditionError, got %T: %v", err, err)
	require.NotEmpty(r.T, preErr.EnabledShares, "EnabledShares must list at least one share name")
	return preErr.EnabledShares
}

// ListRecords returns all backup records for repoName; fails on API error.
func (r *MetadataBackupRunner) ListRecords(repoName string) []apiclient.BackupRecord {
	r.T.Helper()
	recs, err := r.Client.ListBackupRecords(r.StoreName, repoName)
	require.NoError(r.T, err, "ListBackupRecords failed")
	return recs
}

// WaitForBackupRecordSucceeded polls ListRecords until a record with
// status=="succeeded" appears, or timeout elapses. Returns the first
// such record. Fails if none within timeout.
func (r *MetadataBackupRunner) WaitForBackupRecordSucceeded(repoName string, timeout time.Duration) *apiclient.BackupRecord {
	r.T.Helper()
	var found *apiclient.BackupRecord
	require.Eventually(r.T, func() bool {
		for _, rec := range r.ListRecords(repoName) {
			if rec.Status == "succeeded" {
				rec := rec
				found = &rec
				return true
			}
		}
		return false
	}, timeout, 500*time.Millisecond, "no succeeded record in repo %s within %s", repoName, timeout)
	return found
}

// ListLocalstackMultipartUploads queries Localstack for in-flight MPUs
// in the given bucket. Used by chaos tests to assert ghost MPU cleanup
// (DRV-02). Returns an empty slice if the bucket has no pending uploads.
func ListLocalstackMultipartUploads(t *testing.T, lsHelper *framework.LocalstackHelper, bucket string) []s3types.MultipartUpload {
	t.Helper()
	out, err := lsHelper.Client.ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{
		Bucket: aws.String(bucket),
	})
	require.NoError(t, err, "ListMultipartUploads failed")
	return out.Uploads
}
