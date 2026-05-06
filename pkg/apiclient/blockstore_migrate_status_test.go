package apiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateStatus_RoundTrip verifies the dfsctl-bound MigrateStatus method
// issues GET /api/v1/blockstore/migrate/status?share=NAME and decodes the
// MigrateStatusResponse intact.
func TestMigrateStatus_RoundTrip(t *testing.T) {
	want := MigrateStatusResponse{
		Share:           "myshare",
		BlockLayout:     "legacy",
		FilesTotal:      42,
		FilesDone:       17,
		FilesSkipped:    1,
		BytesUploaded:   1024 * 1024,
		BytesDeduped:    512,
		JournalPresent:  true,
		SnapshotPresent: false,
		LastCommitAt:    "2026-05-05T17:30:08Z",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.True(t, strings.HasPrefix(r.URL.Path, "/api/v1/blockstore/migrate/status"),
			"path: %s", r.URL.Path)
		assert.Equal(t, "myshare", r.URL.Query().Get("share"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	got, err := client.MigrateStatus("myshare")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)
}

// TestMigrateStatus_RequiresShare asserts MigrateStatus refuses an empty
// share parameter without issuing an HTTP call.
func TestMigrateStatus_RequiresShare(t *testing.T) {
	client := New("http://example.invalid")
	_, err := client.MigrateStatus("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "share is required")
}

// TestMigrateStatus_404 maps the server's 404 to an APIError with
// IsNotFound()==true so the CLI can surface a friendly message.
func TestMigrateStatus_404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title":  "Not Found",
			"status": 404,
			"detail": "share \"unknown\" not found",
		})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	_, err := client.MigrateStatus("unknown")
	require.Error(t, err)
	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T (%v)", err, err)
	assert.True(t, apiErr.IsNotFound() || apiErr.StatusCode == 404)
}

// TestMigrateStatus_PathEscape asserts that share names with reserved
// URL characters are escaped on the wire so the server sees the original
// share name.
func TestMigrateStatus_PathEscape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "name with spaces", r.URL.Query().Get("share"))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(MigrateStatusResponse{Share: "name with spaces"})
	}))
	defer server.Close()

	client := New(server.URL).WithToken("test-token")
	got, err := client.MigrateStatus("name with spaces")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "name with spaces", got.Share)
}
