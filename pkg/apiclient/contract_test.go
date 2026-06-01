package apiclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/internal/controlplane/api/handlers"
)

// TestContract_DecodesRealServerProblemJSON proves the apiclient decodes the
// canonical RFC 7807 problem+json shape the server actually emits. The
// httptest handler calls the SAME helpers the production handlers use
// (handlers.Conflict / NotFound / Unauthorized / PreconditionFailed), so a
// drift between the wire shape and APIError's tags fails here. This is the
// regression guard for the bug where the client decoded legacy
// {code,message} and silently swallowed every typed error.
func TestContract_DecodesRealServerProblemJSON(t *testing.T) {
	tests := []struct {
		name   string
		emit   func(w http.ResponseWriter, detail string)
		detail string
		status int
		check  func(t *testing.T, e *APIError)
	}{
		{
			name:   "Conflict",
			emit:   handlers.Conflict,
			detail: "resource already exists",
			status: http.StatusConflict,
			check: func(t *testing.T, e *APIError) {
				assert.True(t, e.IsConflict(), "IsConflict() must be true for 409")
			},
		},
		{
			name:   "NotFound",
			emit:   handlers.NotFound,
			detail: "resource not found",
			status: http.StatusNotFound,
			check: func(t *testing.T, e *APIError) {
				assert.True(t, e.IsNotFound(), "IsNotFound() must be true for 404")
			},
		},
		{
			name:   "Unauthorized",
			emit:   handlers.Unauthorized,
			detail: "invalid credentials",
			status: http.StatusUnauthorized,
			check: func(t *testing.T, e *APIError) {
				assert.True(t, e.IsAuthError(), "IsAuthError() must be true for 401")
			},
		},
		{
			name:   "PreconditionFailed",
			emit:   handlers.PreconditionFailed,
			detail: "snapshot is not remotely durable",
			status: http.StatusPreconditionFailed,
			check: func(t *testing.T, e *APIError) {
				assert.Equal(t, http.StatusPreconditionFailed, e.StatusCode)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.emit(w, tc.detail)
			}))
			defer server.Close()

			client := New(server.URL)
			err := client.get("/test", nil)
			require.Error(t, err)

			apiErr, ok := err.(*APIError)
			require.True(t, ok, "expected *APIError, got %T (%v)", err, err)
			assert.Equal(t, tc.status, apiErr.StatusCode, "StatusCode must match HTTP status")
			assert.Equal(t, tc.detail, apiErr.Detail, "Detail must round-trip from problem+json")
			tc.check(t, apiErr)
		})
	}
}
