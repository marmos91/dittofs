package apiclient

import (
	"fmt"
	"net/url"

	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
)

// SnapshotPolicy is the wire DTO for a per-share snapshot policy.
type SnapshotPolicy = dto.SnapshotPolicy

// UpsertSnapshotPolicyRequest mirrors the wire DTO.
type UpsertSnapshotPolicyRequest = dto.UpsertSnapshotPolicyRequest

func snapshotPolicyPath(share string) string {
	return fmt.Sprintf("/api/v1/shares/%s/snapshot-policy", url.PathEscape(normalizeShareNameForAPI(share)))
}

// UpsertSnapshotPolicy creates or updates the snapshot policy for a share.
// Returns the persisted policy.
func (c *Client) UpsertSnapshotPolicy(share string, req UpsertSnapshotPolicyRequest) (*SnapshotPolicy, error) {
	return updateResource[SnapshotPolicy](c, snapshotPolicyPath(share), req)
}

// GetSnapshotPolicy returns the snapshot policy for a share (404 → APIError).
func (c *Client) GetSnapshotPolicy(share string) (*SnapshotPolicy, error) {
	return getResource[SnapshotPolicy](c, snapshotPolicyPath(share))
}

// DeleteSnapshotPolicy removes a share's snapshot policy.
func (c *Client) DeleteSnapshotPolicy(share string) error {
	return deleteResource(c, snapshotPolicyPath(share))
}

// ListSnapshotPolicies returns every snapshot policy across all shares.
// The slice is empty (never nil) when none exist.
func (c *Client) ListSnapshotPolicies() ([]SnapshotPolicy, error) {
	policies, err := listResources[SnapshotPolicy](c, "/api/v1/snapshot-policies")
	if err != nil {
		return nil, err
	}
	if policies == nil {
		return []SnapshotPolicy{}, nil
	}
	return policies, nil
}

// RunSnapshotPolicy triggers the share's policy immediately (manual override).
// Returns the new snapshot id.
func (c *Client) RunSnapshotPolicy(share string) (*CreateSnapshotResponse, error) {
	return createResource[CreateSnapshotResponse](c, snapshotPolicyPath(share)+"/run", nil)
}
