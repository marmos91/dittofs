package apiclient

import (
	"net/url"
)

// CreateBackupRepo creates a new backup repo attached to the named store.
func (c *Client) CreateBackupRepo(storeName string, req *BackupRepoRequest) (*BackupRepo, error) {
	var repo BackupRepo
	if err := c.post(storePath(storeName)+"/repos", req, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

// ListBackupRepos returns all backup repos attached to the named store.
func (c *Client) ListBackupRepos(storeName string) ([]BackupRepo, error) {
	return listResources[BackupRepo](c, storePath(storeName)+"/repos")
}

// GetBackupRepo returns a single repo by name.
func (c *Client) GetBackupRepo(storeName, repoName string) (*BackupRepo, error) {
	return getResource[BackupRepo](c, storePath(storeName)+"/repos/"+url.PathEscape(repoName))
}

// UpdateBackupRepo applies a partial update (D-19) to an existing repo.
// Only non-nil fields in req mutate the stored row.
func (c *Client) UpdateBackupRepo(storeName, repoName string, req *BackupRepoRequest) (*BackupRepo, error) {
	var repo BackupRepo
	if err := c.patch(storePath(storeName)+"/repos/"+url.PathEscape(repoName), req, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

// DeleteBackupRepo deletes a repo. When purgeArchives is true, the server
// iterates every record for the repo and Destination.Delete's the archive
// before removing the DB row. Partial failures are surfaced as a typical
// APIError with the failed_record_ids in the body (repo row preserved).
func (c *Client) DeleteBackupRepo(storeName, repoName string, purgeArchives bool) error {
	p := storePath(storeName) + "/repos/" + url.PathEscape(repoName)
	if purgeArchives {
		p += "?purge_archives=true"
	}
	return c.delete(p, nil)
}
