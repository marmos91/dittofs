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

// MetadataBackupRunner wraps *apiclient.Client with backup helpers scoped to a
// single metadata store. Each subtest should construct its own instance bound to
// a uniquely-named store to avoid cross-subtest state sharing.
type MetadataBackupRunner struct {
	T         *testing.T
	Client    *apiclient.Client
	StoreName string
}

func NewMetadataBackupRunner(t *testing.T, client *apiclient.Client, storeName string) *MetadataBackupRunner {
	return &MetadataBackupRunner{T: t, Client: client, StoreName: storeName}
}

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

func (r *MetadataBackupRunner) TriggerBackup(repoName string) *apiclient.TriggerBackupResponse {
	r.T.Helper()
	resp, err := r.Client.TriggerBackup(r.StoreName, &apiclient.TriggerBackupRequest{Repo: repoName})
	require.NoError(r.T, err, "TriggerBackup failed")
	require.NotNil(r.T, resp, "TriggerBackup must return a non-nil response")
	require.NotNil(r.T, resp.Job, "TriggerBackup must return a Job")
	return resp
}

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

// StartRestore returns the error so callers can assert on *apiclient.RestorePreconditionError.
func (r *MetadataBackupRunner) StartRestore(fromBackupID string) (*apiclient.BackupJob, error) {
	r.T.Helper()
	return r.Client.StartRestore(r.StoreName, &apiclient.RestoreRequest{FromBackupID: fromBackupID})
}

func (r *MetadataBackupRunner) StartRestoreMustSucceed(fromBackupID string) *apiclient.BackupJob {
	r.T.Helper()
	job, err := r.StartRestore(fromBackupID)
	require.NoError(r.T, err, "StartRestore must succeed; enable-share precondition already cleared?")
	return job
}

func (r *MetadataBackupRunner) StartRestoreExpectPrecondition(fromBackupID string) []string {
	r.T.Helper()
	_, err := r.StartRestore(fromBackupID)
	require.Error(r.T, err, "StartRestore must 409 when shares enabled")
	var preErr *apiclient.RestorePreconditionError
	require.True(r.T, errors.As(err, &preErr), "err must be *RestorePreconditionError, got %T: %v", err, err)
	require.NotEmpty(r.T, preErr.EnabledShares, "EnabledShares must list at least one share name")
	return preErr.EnabledShares
}

func (r *MetadataBackupRunner) ListRecords(repoName string) []apiclient.BackupRecord {
	r.T.Helper()
	recs, err := r.Client.ListBackupRecords(r.StoreName, repoName)
	require.NoError(r.T, err, "ListBackupRecords failed")
	return recs
}

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

func ListLocalstackMultipartUploads(t *testing.T, lsHelper *framework.LocalstackHelper, bucket string) []s3types.MultipartUpload {
	t.Helper()
	out, err := lsHelper.Client.ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{
		Bucket: aws.String(bucket),
	})
	require.NoError(t, err, "ListMultipartUploads failed")
	return out.Uploads
}
