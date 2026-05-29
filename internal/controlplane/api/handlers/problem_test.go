package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteJSONAccepted asserts the 202 helper writes the expected status,
// Content-Type, and JSON-encoded body.
func TestWriteJSONAccepted(t *testing.T) {
	type payload struct {
		Hello string `json:"hello"`
	}
	rr := httptest.NewRecorder()
	WriteJSONAccepted(rr, payload{Hello: "world"})

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusAccepted)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got payload
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Hello != "world" {
		t.Fatalf("body.hello = %q, want world", got.Hello)
	}
}

// TestPreconditionFailed asserts the 412 helper writes the expected status,
// Content-Type, and problem+json body.
func TestPreconditionFailed(t *testing.T) {
	rr := httptest.NewRecorder()
	PreconditionFailed(rr, "must be durable")

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPreconditionFailed)
	}
	if ct := rr.Header().Get("Content-Type"); ct != ContentTypeProblemJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, ContentTypeProblemJSON)
	}
	var p Problem
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if p.Status != http.StatusPreconditionFailed {
		t.Fatalf("body.status = %d, want %d", p.Status, http.StatusPreconditionFailed)
	}
	if p.Detail != "must be durable" {
		t.Fatalf("body.detail = %q, want %q", p.Detail, "must be durable")
	}
	if p.Title == "" {
		t.Fatalf("body.title is empty")
	}
}

// TestGatewayTimeout asserts the 504 helper writes the expected status,
// Content-Type, and problem+json body.
func TestGatewayTimeout(t *testing.T) {
	rr := httptest.NewRecorder()
	GatewayTimeout(rr, "drain timed out")

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusGatewayTimeout)
	}
	if ct := rr.Header().Get("Content-Type"); ct != ContentTypeProblemJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, ContentTypeProblemJSON)
	}
	var p Problem
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if p.Status != http.StatusGatewayTimeout {
		t.Fatalf("body.status = %d, want %d", p.Status, http.StatusGatewayTimeout)
	}
	if p.Detail != "drain timed out" {
		t.Fatalf("body.detail = %q, want %q", p.Detail, "drain timed out")
	}
}

// TestConflictExistingHelper asserts the pre-existing 409 helper still
// produces a problem+json body with the right status. Sanity check around
// the helper cluster the new ones live alongside.
func TestConflictExistingHelper(t *testing.T) {
	rr := httptest.NewRecorder()
	Conflict(rr, "share is enabled")

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusConflict)
	}
	if ct := rr.Header().Get("Content-Type"); ct != ContentTypeProblemJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, ContentTypeProblemJSON)
	}
	var p Problem
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if p.Status != http.StatusConflict {
		t.Fatalf("body.status = %d, want %d", p.Status, http.StatusConflict)
	}
}
