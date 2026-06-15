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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpsertSnapshotPolicy_PUTsBody(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody UpsertSnapshotPolicyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewDittoFSClient(srv.URL)
	enabled := true
	err := c.UpsertSnapshotPolicy(context.Background(), "/archive", UpsertSnapshotPolicyRequest{
		Interval: "@daily", KeepLast: 7, TTL: "720h", Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("UpsertSnapshotPolicy: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if gotPath != "/api/v1/shares/%2Farchive/snapshot-policy" {
		t.Errorf("path = %s, want share name escaped", gotPath)
	}
	if gotBody.Interval != "@daily" || gotBody.KeepLast != 7 || gotBody.TTL != "720h" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestUpsertSnapshotPolicy_ShareNotFoundIsTypedNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"share not found","status":404}`))
	}))
	defer srv.Close()

	c := NewDittoFSClient(srv.URL)
	err := c.UpsertSnapshotPolicy(context.Background(), "/missing", UpsertSnapshotPolicyRequest{Interval: "24h"})
	var apiErr *DittoFSAPIError
	if !errors.As(err, &apiErr) || !apiErr.IsNotFound() {
		t.Fatalf("err = %v, want typed 404 DittoFSAPIError", err)
	}
}
