package snapshot

import (
	"context"
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
