package apiclient

import (
	"net/http"
	"net/url"
	"strconv"
)

// ListBackupJobs lists backup/restore jobs for the given store, filtered by
// the parameters in filter. Empty filter fields mean "no filter"; Limit=0
// uses the server default (50, capped at 200).
func (c *Client) ListBackupJobs(storeName string, filter BackupJobFilter) ([]BackupJob, error) {
	p := storePath(storeName) + "/backup-jobs"
	q := url.Values{}
	if filter.RepoName != "" {
		q.Set("repo", filter.RepoName)
	}
	if filter.Status != "" {
		q.Set("status", filter.Status)
	}
	if filter.Kind != "" {
		q.Set("kind", filter.Kind)
	}
	if filter.Limit > 0 {
		q.Set("limit", strconv.Itoa(filter.Limit))
	}
	if enc := q.Encode(); enc != "" {
		p += "?" + enc
	}
	return listResources[BackupJob](c, p)
}

// GetBackupJob fetches a single job by ID (polling endpoint).
func (c *Client) GetBackupJob(storeName, jobID string) (*BackupJob, error) {
	return getResource[BackupJob](c, storePath(storeName)+"/backup-jobs/"+url.PathEscape(jobID))
}

// CancelBackupJob requests cancellation of an in-flight job. Terminal jobs
// return 200 OK + current job (idempotent per D-45); running jobs return 202
// + the (possibly fresh) job row. Both paths deserialize into *BackupJob.
func (c *Client) CancelBackupJob(storeName, jobID string) (*BackupJob, error) {
	var job BackupJob
	if err := c.doWithTypedProblem(http.MethodPost,
		storePath(storeName)+"/backup-jobs/"+url.PathEscape(jobID)+"/cancel",
		nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}
