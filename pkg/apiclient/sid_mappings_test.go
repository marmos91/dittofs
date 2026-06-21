package apiclient

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListSIDMappings_RoundTrip verifies ListSIDMappings hits GET /sid-mappings
// and decodes the mapping array, including user/group distinction.
func TestListSIDMappings_RoundTrip(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()

	body, err := json.Marshal([]SIDMapping{
		{SID: "S-1-5-21-1-2-3-1107", UnixID: 70001, IsGroup: false, DisplayName: "alice", CreatedAt: "2026-06-20T10:00:00Z"},
		{SID: "S-1-5-21-1-2-3-1108", UnixID: 80001, IsGroup: true, DisplayName: "engineers", CreatedAt: "2026-06-20T11:00:00Z"},
	})
	require.NoError(t, err)
	s.body = body

	client := newTestClient(s).WithToken("test-token")
	got, err := client.ListSIDMappings()
	require.NoError(t, err)
	require.Len(t, got, 2)

	calls := s.observedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodGet, calls[0].Method)
	assert.Equal(t, "/api/v1/sid-mappings", calls[0].Path)

	assert.Equal(t, "S-1-5-21-1-2-3-1107", got[0].SID)
	assert.Equal(t, uint32(70001), got[0].UnixID)
	assert.False(t, got[0].IsGroup)
	assert.Equal(t, "alice", got[0].DisplayName)
	assert.True(t, got[1].IsGroup)
	assert.Equal(t, uint32(80001), got[1].UnixID)
}

// TestDeleteSIDMapping_RoundTrip verifies DeleteSIDMapping issues a DELETE to the
// path-escaped SID endpoint and treats success as no error.
func TestDeleteSIDMapping_RoundTrip(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.status = http.StatusNoContent

	client := newTestClient(s).WithToken("test-token")
	err := client.DeleteSIDMapping("S-1-5-21-1-2-3-1107")
	require.NoError(t, err)

	calls := s.observedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodDelete, calls[0].Method)
	assert.Equal(t, "/api/v1/sid-mappings/S-1-5-21-1-2-3-1107", calls[0].Path)
}

// TestDeleteSIDMapping_NotFound maps a 404 to a not-found APIError.
func TestDeleteSIDMapping_NotFound(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.status = http.StatusNotFound
	s.contentType = "application/problem+json"
	s.body = []byte(`{"type":"about:blank","title":"Not Found","status":404,"detail":"SID mapping not found"}`)

	client := newTestClient(s).WithToken("test-token")
	err := client.DeleteSIDMapping("S-1-5-21-9-9-9-9999")
	require.Error(t, err)

	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError, got %T", err)
	assert.True(t, apiErr.IsNotFound(), "404 must surface as IsNotFound()")
}
