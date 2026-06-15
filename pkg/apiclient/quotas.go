package apiclient

import (
	"fmt"
	"net/url"
	"time"
)

// Quota represents a per-identity (user/group/default-user) storage quota on a
// share, mirroring the server QuotaResponse.
type Quota struct {
	ShareName  string  `json:"share_name"`
	Scope      string  `json:"scope"`
	IdentityID *uint32 `json:"identity_id,omitempty"`
	// LimitBytes / SoftBytes are human-readable (e.g. "10 GiB"), empty when
	// unlimited / no soft threshold.
	LimitBytes     string     `json:"limit_bytes,omitempty"`
	SoftBytes      string     `json:"soft_bytes,omitempty"`
	LimitFiles     int64      `json:"limit_files"`
	SoftFiles      int64      `json:"soft_files"`
	GraceSeconds   int64      `json:"grace_seconds"`
	GraceStartedAt *time.Time `json:"grace_started_at,omitempty"`
	UsedBytes      int64      `json:"used_bytes"`
	UsedFiles      int64      `json:"used_files"`
}

// UpsertQuotaRequest is the request body to create or update a quota. Byte
// ceilings are human-readable (e.g. "10GiB"); file/grace fields are integers.
type UpsertQuotaRequest struct {
	LimitBytes   string `json:"limit_bytes,omitempty"`
	SoftBytes    string `json:"soft_bytes,omitempty"`
	LimitFiles   int64  `json:"limit_files,omitempty"`
	SoftFiles    int64  `json:"soft_files,omitempty"`
	GraceSeconds int64  `json:"grace_seconds,omitempty"`
}

// quotaPath builds the REST path for a quota. The default-user scope has no id
// segment; user/group encode the uid/gid in the path.
func quotaPath(share, scope string, id *uint32) string {
	base := fmt.Sprintf("/api/v1/shares/%s/quotas", url.PathEscape(normalizeShareNameForAPI(share)))
	if scope == "default-user" {
		return base + "/default-user"
	}
	idSeg := ""
	if id != nil {
		idSeg = fmt.Sprintf("%d", *id)
	}
	return fmt.Sprintf("%s/%s/%s", base, url.PathEscape(scope), idSeg)
}

// ListQuotas returns all quotas for a share.
func (c *Client) ListQuotas(share string) ([]Quota, error) {
	return listResources[Quota](c, fmt.Sprintf("/api/v1/shares/%s/quotas", url.PathEscape(normalizeShareNameForAPI(share))))
}

// GetQuota returns a single quota by (share, scope, identity). id is nil for the
// default-user scope.
func (c *Client) GetQuota(share, scope string, id *uint32) (*Quota, error) {
	var q Quota
	if err := c.get(quotaPath(share, scope, id), &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// SetQuota creates or updates a quota (PUT). id is nil for the default-user
// scope.
func (c *Client) SetQuota(share, scope string, id *uint32, req *UpsertQuotaRequest) (*Quota, error) {
	var q Quota
	if err := c.put(quotaPath(share, scope, id), req, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// DeleteQuota removes a quota by (share, scope, identity). id is nil for the
// default-user scope.
func (c *Client) DeleteQuota(share, scope string, id *uint32) error {
	return c.delete(quotaPath(share, scope, id), nil)
}
