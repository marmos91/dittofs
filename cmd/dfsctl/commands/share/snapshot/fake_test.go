package snapshot

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// fakeClient is a snapshotClient implementation backed by in-memory maps
// for unit tests. Test-only.
type fakeClient struct {
	share          *apiclient.Share
	getShareErr    error
	snapshots      map[string]*apiclient.Snapshot
	createResp     *apiclient.CreateSnapshotResponse
	createErr      error
	restoreResp    *apiclient.RestoreSnapshotResponse
	restoreErr     error
	deleteCalls    []string
	restoreReq     *apiclient.RestoreSnapshotRequest
	listOverride   []apiclient.Snapshot
	listErr        error
	waitFinalState string
}

func (f *fakeClient) CreateSnapshot(share string, req apiclient.CreateSnapshotRequest) (*apiclient.CreateSnapshotResponse, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResp != nil {
		return f.createResp, nil
	}
	return &apiclient.CreateSnapshotResponse{SnapshotID: "snap-new", Share: share}, nil
}

func (f *fakeClient) ListSnapshots(share string) ([]apiclient.Snapshot, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listOverride != nil {
		return f.listOverride, nil
	}
	out := make([]apiclient.Snapshot, 0, len(f.snapshots))
	for _, s := range f.snapshots {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeClient) GetSnapshot(share, id string) (*apiclient.Snapshot, error) {
	if s, ok := f.snapshots[id]; ok {
		return s, nil
	}
	return nil, &apiclient.APIError{Code: "NOT_FOUND", Message: "snapshot not found", StatusCode: 404}
}

func (f *fakeClient) DeleteSnapshot(share, id string) error {
	f.deleteCalls = append(f.deleteCalls, id)
	delete(f.snapshots, id)
	return nil
}

func (f *fakeClient) RestoreSnapshot(share, id string, req apiclient.RestoreSnapshotRequest) (*apiclient.RestoreSnapshotResponse, error) {
	f.restoreReq = &req
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	if f.restoreResp != nil {
		return f.restoreResp, nil
	}
	return &apiclient.RestoreSnapshotResponse{SnapshotID: id, Share: share, SafetySnapshotID: "safety-xyz"}, nil
}

func (f *fakeClient) WaitForSnapshot(ctx context.Context, share, id string, pollEvery time.Duration) (*apiclient.Snapshot, error) {
	s, ok := f.snapshots[id]
	if !ok {
		return nil, fmt.Errorf("snapshot %s not in fake", id)
	}
	if f.waitFinalState != "" {
		copy := *s
		copy.State = f.waitFinalState
		return &copy, nil
	}
	return s, nil
}

func (f *fakeClient) GetShare(name string) (*apiclient.Share, error) {
	if f.getShareErr != nil {
		return nil, f.getShareErr
	}
	if f.share != nil {
		return f.share, nil
	}
	return &apiclient.Share{Name: name, Enabled: true}, nil
}

// withFakeClient swaps getClient for a fake during a test, restoring the
// previous value on cleanup.
func withFakeClient(t interface {
	Cleanup(func())
}, fc *fakeClient) {
	orig := getClient
	getClient = func() (snapshotClient, error) { return fc, nil }
	t.Cleanup(func() { getClient = orig })
}
