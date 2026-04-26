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

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
)

// TestBlockStoreGC_RoundTrip verifies the dfsctl-bound BlockStoreGC
// method posts to the per-share GC endpoint with the dry_run body and
// decodes the BlockStoreGCResult correctly.
func TestBlockStoreGC_RoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/shares/myshare/blockstore/gc", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var got BlockStoreGCOptions
		require.NoError(t, json.Unmarshal(body, &got))
		assert.True(t, got.DryRun, "dry_run flag must propagate")

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(BlockStoreGCResult{
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
	res, err := client.BlockStoreGC("myshare", &BlockStoreGCOptions{DryRun: true})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Stats)
	assert.Equal(t, int64(12), res.Stats.HashesMarked)
	assert.Equal(t, int64(3), res.Stats.ObjectsSwept)
	assert.True(t, res.Stats.DryRun)
	assert.Equal(t, []string{"cas/aa/bb/abc"}, res.Stats.DryRunCandidates)
}

// TestBlockStoreGC_NilOpts verifies that passing nil opts maps to a
// dry_run=false body — matching the documented "nil = non-dry-run"
// contract.
func TestBlockStoreGC_NilOpts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var got BlockStoreGCOptions
		require.NoError(t, json.Unmarshal(body, &got))
		assert.False(t, got.DryRun, "nil opts must produce dry_run=false")

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(BlockStoreGCResult{Stats: &engine.GCStats{}})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	_, err := client.BlockStoreGC("myshare", nil)
	require.NoError(t, err)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(APIError{
			Code:    "NOT_FOUND",
			Message: "no GC run recorded for share myshare",
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
