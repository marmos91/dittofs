package snapshot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// snapshotClient is the narrow interface the leaf commands use. Tests
// replace getClient with a fake that satisfies this interface; production
// uses cmdutil.GetAuthenticatedClient which returns *apiclient.Client.
type snapshotClient interface {
	CreateSnapshot(share string, req apiclient.CreateSnapshotRequest) (*apiclient.CreateSnapshotResponse, error)
	ListSnapshots(share string) ([]apiclient.Snapshot, error)
	GetSnapshot(share, id string) (*apiclient.Snapshot, error)
	DeleteSnapshot(share, id string) error
	RestoreSnapshot(share, id string, req apiclient.RestoreSnapshotRequest) (*apiclient.RestoreSnapshotResponse, error)
	WaitForSnapshot(ctx context.Context, share, id string, pollEvery time.Duration) (*apiclient.Snapshot, error)
	GetShare(name string) (*apiclient.Share, error)
}

// getClient is overridable by tests. The default returns the configured
// authenticated client from the credential store.
var getClient = func() (snapshotClient, error) {
	return cmdutil.GetAuthenticatedClient()
}

// resolveSnapshotID resolves a possibly-partial snapshot id to a full id by
// unique prefix match against the share's snapshots (git-style). An exact
// match always wins. A unique prefix resolves; an ambiguous or unknown
// prefix is a clear error. This lets operators paste the 8-char id printed
// by `list` into show/delete/restore/--retry.
func resolveSnapshotID(client snapshotClient, share, partial string) (string, error) {
	if partial == "" {
		return "", fmt.Errorf("snapshot id is required")
	}
	snaps, err := client.ListSnapshots(share)
	if err != nil {
		return "", fmt.Errorf("failed to list snapshots for id resolution: %w", err)
	}
	var matches []string
	for _, s := range snaps {
		if s.ID == partial {
			return s.ID, nil // exact match short-circuits ambiguity
		}
		if strings.HasPrefix(s.ID, partial) {
			matches = append(matches, s.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no snapshot on share %q matches id %q", share, partial)
	default:
		return "", fmt.Errorf("snapshot id %q is ambiguous on share %q (%d matches: %s)",
			partial, share, len(matches), strings.Join(matches, ", "))
	}
}
