package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time assertions that the local types are aliases of dto.*.
// Functions that take one type only accept the other if the two are the
// same Go type (alias), not just structurally identical — so the
// compiler rejects any future drift between the two declarations.
func acceptDtoSnapshot(dto.Snapshot)                       {}
func acceptDtoCreateRequest(dto.CreateSnapshotRequest)     {}
func acceptDtoCreateResponse(dto.CreateSnapshotResponse)   {}
func acceptDtoRestoreRequest(dto.RestoreSnapshotRequest)   {}
func acceptDtoRestoreResponse(dto.RestoreSnapshotResponse) {}

func TestSnapshot_DTOAliases(t *testing.T) {
	acceptDtoSnapshot(Snapshot{})
	acceptDtoCreateRequest(CreateSnapshotRequest{})
	acceptDtoCreateResponse(CreateSnapshotResponse{})
	acceptDtoRestoreRequest(RestoreSnapshotRequest{})
	acceptDtoRestoreResponse(RestoreSnapshotResponse{})
}

func TestCreateSnapshot(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.status = http.StatusAccepted
	s.body, _ = json.Marshal(CreateSnapshotResponse{
		SnapshotID: "snap-123",
		Share:      "/archive",
	})

	c := newTestClient(s)
	resp, err := c.CreateSnapshot("/archive", CreateSnapshotRequest{Name: "weekly", NoVerify: true, RetryOf: "snap-prev"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "snap-123", resp.SnapshotID)
	assert.Equal(t, "/archive", resp.Share)

	calls := s.observedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodPost, calls[0].Method)
	assert.Equal(t, "/api/v1/shares/archive/snapshots", calls[0].Path)
	// Body propagates request fields.
	var sent CreateSnapshotRequest
	require.NoError(t, json.Unmarshal(calls[0].Body, &sent))
	assert.Equal(t, "weekly", sent.Name)
	assert.True(t, sent.NoVerify)
	assert.Equal(t, "snap-prev", sent.RetryOf)
}

func TestListSnapshots(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.body, _ = json.Marshal([]Snapshot{
		{ID: "a", Name: "n1", Share: "/archive", State: "ready", RemoteDurable: true},
		{ID: "b", Name: "n2", Share: "/archive", State: "creating"},
	})

	c := newTestClient(s)
	got, err := c.ListSnapshots("/archive")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ID)
	assert.Equal(t, "ready", got[0].State)
	assert.True(t, got[0].RemoteDurable)
	assert.Equal(t, "n2", got[1].Name)

	calls := s.observedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodGet, calls[0].Method)
	assert.Equal(t, "/api/v1/shares/archive/snapshots", calls[0].Path)
}

func TestListSnapshots_Empty(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.body = []byte(`[]`)

	c := newTestClient(s)
	got, err := c.ListSnapshots("/archive")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGetSnapshot(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.body, _ = json.Marshal(Snapshot{
		ID: "snap-xyz", Share: "/archive", State: "ready",
		ManifestCount: 5, DumpBytes: 1024,
	})

	c := newTestClient(s)
	snap, err := c.GetSnapshot("/archive", "snap-xyz")
	require.NoError(t, err)
	require.NotNil(t, snap)
	assert.Equal(t, "snap-xyz", snap.ID)
	assert.Equal(t, 5, snap.ManifestCount)
	assert.Equal(t, int64(1024), snap.DumpBytes)

	calls := s.observedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "/api/v1/shares/archive/snapshots/snap-xyz", calls[0].Path)
}

func TestGetSnapshot_NotFound(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.status = http.StatusNotFound
	s.body, _ = json.Marshal(APIError{Code: "NOT_FOUND", Message: "snapshot not found"})

	c := newTestClient(s)
	snap, err := c.GetSnapshot("/archive", "missing")
	require.Error(t, err)
	assert.Nil(t, snap)
	apiErr, ok := err.(*APIError)
	require.True(t, ok)
	assert.True(t, apiErr.IsNotFound())
}

func TestDeleteSnapshot(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.status = http.StatusNoContent

	c := newTestClient(s)
	err := c.DeleteSnapshot("/archive", "snap-1")
	require.NoError(t, err)

	calls := s.observedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodDelete, calls[0].Method)
	assert.Equal(t, "/api/v1/shares/archive/snapshots/snap-1", calls[0].Path)
}

func TestDeleteSnapshot_NotFound(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.status = http.StatusNotFound
	s.body, _ = json.Marshal(APIError{Code: "NOT_FOUND", Message: "snapshot not found"})

	c := newTestClient(s)
	err := c.DeleteSnapshot("/archive", "missing")
	require.Error(t, err)
}

func TestRestoreSnapshot(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.body, _ = json.Marshal(RestoreSnapshotResponse{
		SnapshotID:       "snap-xyz",
		Share:            "/archive",
		SafetySnapshotID: "safety-abc",
	})

	c := newTestClient(s)
	resp, err := c.RestoreSnapshot("/archive", "snap-xyz", RestoreSnapshotRequest{AllowNonDurable: true})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "snap-xyz", resp.SnapshotID)
	assert.Equal(t, "safety-abc", resp.SafetySnapshotID)
	assert.Equal(t, "/archive", resp.Share)

	calls := s.observedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodPost, calls[0].Method)
	assert.Equal(t, "/api/v1/shares/archive/snapshots/snap-xyz/restore", calls[0].Path)
	var sent RestoreSnapshotRequest
	require.NoError(t, json.Unmarshal(calls[0].Body, &sent))
	assert.True(t, sent.AllowNonDurable)
}

// TestRestoreSnapshot_UsesRestoreTimeoutNotBaseClient is the behavioral
// regression for #842: a remote-backed restore whose server-side
// safety-snapshot drain runs longer than the base 30s http.Client timeout
// must NOT be killed client-side. The fix routes RestoreSnapshot through a
// per-call client whose Timeout is restoreHTTPTimeout (30m default), not the
// 30s base client.
//
// We prove the restore call is governed by restoreHTTPTimeout — not the base
// 30s client — without waiting 30s: point a SHORT restore timeout at a server
// that delays slightly longer and assert it times out. If restore wrongly
// used the 30s base client, the short delay would succeed; the timeout error
// proves restoreHTTPTimeout is the authority.
func TestRestoreSnapshot_UsesRestoreTimeoutNotBaseClient(t *testing.T) {
	const serverDelay = 250 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RestoreSnapshotResponse{SnapshotID: "s", Share: "/a"})
	}))
	defer srv.Close()

	// (a) Restore timeout SHORTER than the server delay -> must fail. This is
	// only possible because the restore call honors restoreHTTPTimeout; the
	// 30s base client would have waited out the 250ms delay and succeeded.
	cShort := New(srv.URL, WithRestoreTimeout(40*time.Millisecond))
	_, err := cShort.RestoreSnapshot("/a", "s", RestoreSnapshotRequest{})
	require.Error(t, err, "restore must honor the short restoreHTTPTimeout, not the 30s base client")

	// (b) Restore timeout LONGER than the server delay -> must succeed,
	// confirming the per-call client is wired and not capped at the base 30s
	// in a way that would matter here.
	cLong := New(srv.URL, WithRestoreTimeout(10*time.Second))
	resp, err := cLong.RestoreSnapshot("/a", "s", RestoreSnapshotRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestRestoreSnapshot_PreconditionFailed(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()

	s.reset()
	s.status = http.StatusPreconditionFailed
	s.body, _ = json.Marshal(APIError{Code: "PRECONDITION_FAILED", Message: "snapshot is not remotely durable"})

	c := newTestClient(s)
	resp, err := c.RestoreSnapshot("/archive", "snap-x", RestoreSnapshotRequest{})
	require.Error(t, err)
	assert.Nil(t, resp)
	apiErr, ok := err.(*APIError)
	require.True(t, ok)
	assert.Equal(t, http.StatusPreconditionFailed, apiErr.StatusCode)
}

func TestWaitForSnapshot_PollsUntilReady(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/shares/archive/snapshots/snap-1", func(w http.ResponseWriter, r *http.Request) {
		calls++
		state := "creating"
		if calls >= 3 {
			state = "ready"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Snapshot{ID: "snap-1", Share: "/archive", State: state})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := c.WaitForSnapshot(ctx, "/archive", "snap-1", 10*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, snap)
	assert.Equal(t, "ready", snap.State)
	assert.GreaterOrEqual(t, calls, 3)
}

func TestWaitForSnapshot_ContextCanceled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/shares/archive/snapshots/snap-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Snapshot{ID: "snap-1", Share: "/archive", State: "creating"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.WaitForSnapshot(ctx, "/archive", "snap-1", 5*time.Millisecond)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "context") || err == context.DeadlineExceeded || err == context.Canceled)
}

func TestWaitForSnapshot_FailedState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/shares/archive/snapshots/snap-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Snapshot{ID: "snap-1", Share: "/archive", State: "failed", Error: "boom"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snap, err := c.WaitForSnapshot(ctx, "/archive", "snap-1", 5*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, snap)
	assert.Equal(t, "failed", snap.State)
	assert.Equal(t, "boom", snap.Error)
}

func TestWithRestoreTimeout_OverridesHTTPTimeout(t *testing.T) {
	c := New("http://example.invalid", WithRestoreTimeout(45*time.Minute))
	assert.Equal(t, 45*time.Minute, c.restoreHTTPTimeout)
}

func TestNew_DefaultRestoreTimeoutIs30Min(t *testing.T) {
	c := New("http://example.invalid")
	assert.Equal(t, 30*time.Minute, c.restoreHTTPTimeout)
}
