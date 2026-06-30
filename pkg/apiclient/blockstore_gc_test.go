package apiclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block/engine"
)

// TestStartBlockStoreGC_RoundTrip verifies the kick-off call posts to the
// per-share GC endpoint with the dry_run body and returns the job id from the
// 202 response.
func TestStartBlockStoreGC_RoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/shares/myshare/blockstore/gc", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var got BlockStoreGCOptions
		require.NoError(t, json.Unmarshal(body, &got))
		assert.True(t, got.DryRun, "dry_run flag must propagate")
		assert.True(t, got.Reconcile, "reconcile flag must propagate")

		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(gcStartResponse{
			JobID:  "gc-9",
			Status: GCJobStatus{ID: "gc-9", State: "running"},
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	jobID, err := client.StartBlockStoreGC("myshare", &BlockStoreGCOptions{DryRun: true, Reconcile: true})
	require.NoError(t, err)
	assert.Equal(t, "gc-9", jobID)
}

// TestStartBlockStoreGC_NilOpts verifies that passing nil opts maps to a
// dry_run=false body — matching the documented "nil = non-dry-run" contract.
func TestStartBlockStoreGC_NilOpts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var got BlockStoreGCOptions
		require.NoError(t, json.Unmarshal(body, &got))
		assert.False(t, got.DryRun, "nil opts must produce dry_run=false")
		assert.False(t, got.Reconcile)

		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(gcStartResponse{JobID: "gc-1", Status: GCJobStatus{ID: "gc-1", State: "running"}})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	_, err := client.StartBlockStoreGC("myshare", nil)
	require.NoError(t, err)
}

// TestGetBlockStoreGCJob_RoundTrip verifies the poll call GETs the job endpoint
// and decodes the terminal status (including the embedded final stats).
func TestGetBlockStoreGCJob_RoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/shares/myshare/blockstore/gc/gc-9", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(GCJobStatus{
			ID:           "gc-9",
			State:        "done",
			HashesMarked: 12,
			ObjectsSwept: 3,
			BytesFreed:   4096,
			Stats: &engine.GCStats{
				HashesMarked:     12,
				ObjectsSwept:     3,
				BytesFreed:       4096,
				DryRun:           true,
				DryRunCandidates: []string{"cas/aa/bb/abc"},
			},
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	got, err := client.GetBlockStoreGCJob("myshare", "gc-9")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "done", got.State)
	assert.Equal(t, int64(3), got.ObjectsSwept)
	require.NotNil(t, got.Stats)
	assert.Equal(t, []string{"cas/aa/bb/abc"}, got.Stats.DryRunCandidates)
}

// TestBlockStoreGCStatus_RoundTrip verifies the GC-status read returns
// the parsed engine.GCRunSummary intact.
func TestBlockStoreGCStatus_RoundTrip(t *testing.T) {
	want := engine.GCRunSummary{
		RunID:        "abc-123",
		StartedAt:    time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC),
		CompletedAt:  time.Date(2026, 4, 25, 10, 0, 1, 0, time.UTC),
		HashesMarked: 5,
		ObjectsSwept: 1,
		BytesFreed:   1024,
		DurationMs:   1000,
		DryRun:       false,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/shares/myshare/blockstore/gc-status", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	got, err := client.BlockStoreGCStatus("myshare")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want.RunID, got.RunID)
	assert.Equal(t, want.HashesMarked, got.HashesMarked)
	assert.Equal(t, want.BytesFreed, got.BytesFreed)
}

// TestBlockStoreGCStatus_NotFound surfaces the 404 (no run yet) as an
// APIError that IsNotFound() returns true for, so callers can detect
// the "no run yet" state without string matching.
func TestBlockStoreGCStatus_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":   "about:blank",
			"title":  "Not Found",
			"status": http.StatusNotFound,
			"detail": "no GC run recorded for share myshare",
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	_, err := client.BlockStoreGCStatus("myshare")
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	assert.True(t, apiErr.IsNotFound(), "404 must surface as IsNotFound()")
}
