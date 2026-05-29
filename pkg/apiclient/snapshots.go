package apiclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
)

// Snapshot is the wire DTO for a share snapshot, re-exported from the
// neutral dto package so callers do not have to import it directly.
type Snapshot = dto.Snapshot

// CreateSnapshotRequest mirrors the wire DTO.
type CreateSnapshotRequest = dto.CreateSnapshotRequest

// CreateSnapshotResponse mirrors the wire DTO.
type CreateSnapshotResponse = dto.CreateSnapshotResponse

// RestoreSnapshotRequest mirrors the wire DTO.
type RestoreSnapshotRequest = dto.RestoreSnapshotRequest

// RestoreSnapshotResponse mirrors the wire DTO. SafetySnapshotID is the ID
// of the pre-restore safety snapshot taken before the destructive reset;
// empty when the precheck or pre-verify step failed.
type RestoreSnapshotResponse = dto.RestoreSnapshotResponse

// snapshotsPath returns the collection path for a share.
func snapshotsPath(share string) string {
	return fmt.Sprintf("/api/v1/shares/%s/snapshots", url.PathEscape(normalizeShareNameForAPI(share)))
}

// snapshotPath returns the resource path for a single snapshot.
func snapshotPath(share, id string) string {
	return fmt.Sprintf("/api/v1/shares/%s/snapshots/%s",
		url.PathEscape(normalizeShareNameForAPI(share)),
		url.PathEscape(id))
}

// CreateSnapshot kicks off a snapshot run. The server responds 202 with
// a CreateSnapshotResponse carrying the new snapshot ID; the snapshot
// transitions through "creating" → "ready"|"failed" asynchronously.
// Use WaitForSnapshot to block until the terminal state is reached.
func (c *Client) CreateSnapshot(share string, req CreateSnapshotRequest) (*CreateSnapshotResponse, error) {
	return createResource[CreateSnapshotResponse](c, snapshotsPath(share), req)
}

// ListSnapshots returns the snapshots known for the share. The slice is
// empty (never nil) when the share has no snapshots.
func (c *Client) ListSnapshots(share string) ([]Snapshot, error) {
	snaps, err := listResources[Snapshot](c, snapshotsPath(share))
	if err != nil {
		return nil, err
	}
	if snaps == nil {
		return []Snapshot{}, nil
	}
	return snaps, nil
}

// GetSnapshot returns the detail view of a single snapshot.
func (c *Client) GetSnapshot(share, id string) (*Snapshot, error) {
	return getResource[Snapshot](c, snapshotPath(share, id))
}

// getSnapshotCtx is the ctx-aware variant used by poll loops so caller
// cancellation aborts the in-flight HTTP request (not just the wait
// between polls).
func (c *Client) getSnapshotCtx(ctx context.Context, share, id string) (*Snapshot, error) {
	var snap Snapshot
	if err := c.getCtx(ctx, snapshotPath(share, id), &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// DeleteSnapshot deletes a snapshot. Server-side: row delete + on-disk
// dir wipe + lock release. 204 → nil; non-2xx → APIError.
func (c *Client) DeleteSnapshot(share, id string) error {
	return deleteResource(c, snapshotPath(share, id))
}

// RestoreSnapshot replays the snapshot's metadata into the share. The
// call runs against a long-timeout http.Client (default 30 minutes,
// overridable via WithRestoreTimeout) because the server-side restore
// can run for many minutes on large shares.
//
// SafetySnapshotID in the response is the ID of the pre-restore safety
// snap (empty when precheck failed before any snap was taken).
func (c *Client) RestoreSnapshot(share, id string, req RestoreSnapshotRequest) (*RestoreSnapshotResponse, error) {
	timeout := c.restoreHTTPTimeout
	if timeout <= 0 {
		timeout = defaultRestoreHTTPTimeout
	}
	var resp RestoreSnapshotResponse
	if err := c.doWithTimeout(http.MethodPost, snapshotPath(share, id)+"/restore", req, &resp, timeout); err != nil {
		return nil, err
	}
	return &resp, nil
}

// WaitForSnapshot polls GetSnapshot every pollEvery until the snapshot
// leaves the "creating" state or ctx is canceled. The terminal snapshot
// (state == "ready" or "failed") is returned; on ctx cancellation
// ctx.Err() propagates.
//
// pollEvery <= 0 is treated as 500ms.
func (c *Client) WaitForSnapshot(ctx context.Context, share, id string, pollEvery time.Duration) (*Snapshot, error) {
	if pollEvery <= 0 {
		pollEvery = 500 * time.Millisecond
	}
	t := time.NewTicker(pollEvery)
	defer t.Stop()

	// First poll happens immediately so callers don't pay a full pollEvery
	// when the snapshot is already terminal.
	for {
		snap, err := c.getSnapshotCtx(ctx, share, id)
		if err != nil {
			return nil, err
		}
		if snap.State != "creating" {
			return snap, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
