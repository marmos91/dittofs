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
)

// trashServer is a recording stub that answers the four trash endpoints,
// capturing the path/method and the decoded request body for assertions.
// Mirrors gc_test.go's newGCServer pattern.
type trashServer struct {
	*httptest.Server
	lastMethod string
	lastPath   string
	lastBody   []byte
	status     int

	entries []TrashEntry
	status_ TrashStatus
	removed int
}

func newTrashServer(t *testing.T) *trashServer {
	t.Helper()
	s := &trashServer{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastMethod = r.Method
		s.lastPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		s.lastBody = body

		w.Header().Set("Content-Type", "application/json")
		if s.status >= 400 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(s.status)
			_, _ = io.WriteString(w, `{"type":"about:blank","title":"Conflict","status":409,"detail":"restore destination already exists"}`)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/shares/export/trash":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(s.entries))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/shares/export/trash/restore":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/shares/export/trash/empty":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(trashEmptyResponse{Removed: s.removed}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/shares/export/trash/status":
			w.WriteHeader(http.StatusOK)
			require.NoError(t, json.NewEncoder(w).Encode(s.status_))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return s
}

// TestTrashList_RoundTrip verifies TrashList hits GET /trash with the
// path-escaped share name and decodes the entry array.
func TestTrashList_RoundTrip(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	deletedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s.entries = []TrashEntry{
		{
			BinPath:      "doc.txt",
			OriginalPath: "a/doc.txt",
			DeletedBy:    "alice",
			DeletedAt:    deletedAt,
			Size:         4096,
			IsDir:        false,
		},
		{
			BinPath:      "olddir",
			OriginalPath: "b/olddir",
			DeletedBy:    "bob",
			DeletedAt:    deletedAt,
			IsDir:        true,
		},
	}

	client := New(s.URL).WithToken("test-token")
	// Leading slash exercises share-name path-escaping (shares can be "/export").
	got, err := client.TrashList("/export")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, http.MethodGet, s.lastMethod)
	assert.Equal(t, "/api/v1/shares/export/trash", s.lastPath)
	assert.Equal(t, "doc.txt", got[0].BinPath)
	assert.Equal(t, "a/doc.txt", got[0].OriginalPath)
	assert.Equal(t, "alice", got[0].DeletedBy)
	assert.Equal(t, deletedAt, got[0].DeletedAt)
	assert.Equal(t, uint64(4096), got[0].Size)
	assert.False(t, got[0].IsDir)
	assert.True(t, got[1].IsDir)
}

// TestTrashRestore_RoundTrip verifies TrashRestore posts the bin_path/to body
// to the restore endpoint and treats 204 as success.
func TestTrashRestore_RoundTrip(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()

	client := New(s.URL).WithToken("test-token")
	err := client.TrashRestore("export", "doc.txt", "restored/doc.txt")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, s.lastMethod)
	assert.Equal(t, "/api/v1/shares/export/trash/restore", s.lastPath)

	var sent trashRestoreRequest
	require.NoError(t, json.Unmarshal(s.lastBody, &sent))
	assert.Equal(t, "doc.txt", sent.BinPath)
	assert.Equal(t, "restored/doc.txt", sent.To)
}

// TestTrashRestore_Conflict maps a 409 to a conflict APIError.
func TestTrashRestore_Conflict(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	s.status = http.StatusConflict

	client := New(s.URL).WithToken("test-token")
	err := client.TrashRestore("export", "doc.txt", "")
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	assert.True(t, apiErr.IsConflict(), "409 must surface as IsConflict()")
}

// TestTrashEmpty_RoundTrip verifies TrashEmpty posts the force flag and decodes
// the removed count.
func TestTrashEmpty_RoundTrip(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	s.removed = 7

	client := New(s.URL).WithToken("test-token")
	removed, err := client.TrashEmpty("export", true)
	require.NoError(t, err)
	assert.Equal(t, 7, removed)
	assert.Equal(t, http.MethodPost, s.lastMethod)
	assert.Equal(t, "/api/v1/shares/export/trash/empty", s.lastPath)

	var sent trashEmptyRequest
	require.NoError(t, json.Unmarshal(s.lastBody, &sent))
	assert.True(t, sent.Force, "force flag must propagate")
}

// TestTrashStatus_RoundTrip verifies TrashStatus reads the status endpoint and
// decodes the roll-up, including the optional oldest timestamp.
func TestTrashStatus_RoundTrip(t *testing.T) {
	s := newTrashServer(t)
	defer s.Close()
	oldest := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	s.status_ = TrashStatus{
		Enabled:    true,
		ItemCount:  3,
		TotalBytes: 12288,
		Oldest:     &oldest,
	}

	client := New(s.URL).WithToken("test-token")
	got, err := client.TrashStatus("export")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, http.MethodGet, s.lastMethod)
	assert.Equal(t, "/api/v1/shares/export/trash/status", s.lastPath)
	assert.True(t, got.Enabled)
	assert.Equal(t, 3, got.ItemCount)
	assert.Equal(t, uint64(12288), got.TotalBytes)
	require.NotNil(t, got.Oldest)
	assert.Equal(t, oldest, *got.Oldest)
}
