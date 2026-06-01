/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// canonicalProblemBodies are the exact RFC 7807 problem+json wire bodies the
// DittoFS server emits. The operator module cannot import the server's
// internal/controlplane/api/handlers package (separate go.mod), so the
// canonical shape is hardcoded here. Source of truth:
//
//	internal/controlplane/api/handlers/problem.go
//	  WriteProblem sets {"type":"about:blank","title":<X>,"status":<N>,"detail":<msg>}
//	  helpers: Conflict / NotFound / Unauthorized / Forbidden / PreconditionFailed / ...
//
// If the server's wire shape drifts, this test must be updated in lockstep —
// that is the contract this guards.
var canonicalProblemBodies = map[string]struct {
	status int
	body   string
}{
	"Conflict": {
		status: http.StatusConflict,
		body:   `{"type":"about:blank","title":"Conflict","status":409,"detail":"resource already exists"}`,
	},
	"NotFound": {
		status: http.StatusNotFound,
		body:   `{"type":"about:blank","title":"Not Found","status":404,"detail":"resource not found"}`,
	},
	"Unauthorized": {
		status: http.StatusUnauthorized,
		body:   `{"type":"about:blank","title":"Unauthorized","status":401,"detail":"invalid credentials"}`,
	},
	"Forbidden": {
		status: http.StatusForbidden,
		body:   `{"type":"about:blank","title":"Forbidden","status":403,"detail":"forbidden"}`,
	},
}

// TestContract_DittoFSAPIErrorDecodesProblemJSON proves the operator's
// DittoFSAPIError decodes the real server problem+json shape and that
// errors.As + the Is* helpers classify correctly on HTTP status. This is the
// regression guard for the bug where the operator decoded legacy
// {code,message} and silently dropped every typed error — which made the
// auth reconciler's conflict-tolerance unreachable.
func TestContract_DittoFSAPIErrorDecodesProblemJSON(t *testing.T) {
	for name, tc := range canonicalProblemBodies {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			c := NewDittoFSClient(server.URL)
			err := c.do(context.Background(), http.MethodGet, "/test", nil, nil)
			if err == nil {
				t.Fatalf("%s: expected error, got nil", name)
			}

			var apiErr *DittoFSAPIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("%s: expected *DittoFSAPIError via errors.As, got %T (%v)", name, err, err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("%s: StatusCode = %d, want %d", name, apiErr.StatusCode, tc.status)
			}

			switch name {
			case "Conflict":
				if !apiErr.IsConflict() {
					t.Errorf("Conflict: IsConflict() = false, want true")
				}
			case "NotFound":
				if !apiErr.IsNotFound() {
					t.Errorf("NotFound: IsNotFound() = false, want true")
				}
			case "Unauthorized", "Forbidden":
				if !apiErr.IsAuthError() {
					t.Errorf("%s: IsAuthError() = false, want true", name)
				}
			}
		})
	}
}

// TestContract_IsTransientError_5xx proves a 5xx problem+json from the server
// is classified transient (so the reconciler retries) while a 4xx is
// terminal. This guards the isTransientError fix that previously returned
// false unconditionally for every *DittoFSAPIError.
func TestContract_IsTransientError_5xx(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusConflict, false},
		{http.StatusNotFound, false},
		{http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		err := &DittoFSAPIError{StatusCode: tc.status, Status: tc.status, Title: "x"}
		if got := isTransientError(err); got != tc.want {
			t.Errorf("isTransientError(status=%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}
