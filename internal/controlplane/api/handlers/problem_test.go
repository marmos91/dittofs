package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteBackupAlreadyRunningProblem verifies the 409 typed problem
// emits the running_job_id field alongside the standard RFC 7807 envelope
// (D-13).
func TestWriteBackupAlreadyRunningProblem(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteBackupAlreadyRunningProblem(rr, "01HABC0000000000000000000")

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusConflict)
	}
	if got := rr.Header().Get("Content-Type"); got != ContentTypeProblemJSON {
		t.Fatalf("Content-Type = %q, want %q", got, ContentTypeProblemJSON)
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["title"] != "Conflict" {
		t.Errorf("title = %v, want Conflict", body["title"])
	}
	// JSON numbers decode to float64 in map[string]any.
	if status, _ := body["status"].(float64); int(status) != http.StatusConflict {
		t.Errorf("status = %v, want 409", body["status"])
	}
	if body["detail"] != "backup already running" {
		t.Errorf("detail = %q, want %q", body["detail"], "backup already running")
	}
	if got := body["running_job_id"]; got != "01HABC0000000000000000000" {
		t.Errorf("running_job_id = %v, want 01HABC...", got)
	}
}

// TestWriteRestorePreconditionFailedProblem verifies the 409 typed problem
// emits the enabled_shares array alongside the standard envelope (D-29).
func TestWriteRestorePreconditionFailedProblem(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteRestorePreconditionFailedProblem(rr, []string{"/a", "/b"})

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusConflict)
	}
	if got := rr.Header().Get("Content-Type"); got != ContentTypeProblemJSON {
		t.Fatalf("Content-Type = %q, want %q", got, ContentTypeProblemJSON)
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["title"] != "Restore precondition failed" {
		t.Errorf("title = %v, want 'Restore precondition failed'", body["title"])
	}
	if body["detail"] != "2 share(s) still enabled" {
		t.Errorf("detail = %q, want '2 share(s) still enabled'", body["detail"])
	}
	enabled, ok := body["enabled_shares"].([]any)
	if !ok {
		t.Fatalf("enabled_shares missing or wrong type: %T", body["enabled_shares"])
	}
	if len(enabled) != 2 || enabled[0] != "/a" || enabled[1] != "/b" {
		t.Errorf("enabled_shares = %v, want [/a /b]", enabled)
	}
}

// TestEmbeddedProblemBackCompat ensures the typed-variant embed flattens
// the Problem base fields (title, status, detail) at the top level of the
// JSON, not nested under a "Problem" key. RFC 7807 readers depend on this.
func TestEmbeddedProblemBackCompat(t *testing.T) {
	p := &BackupAlreadyRunningProblem{
		Problem: Problem{
			Type:   "about:blank",
			Title:  "Conflict",
			Status: http.StatusConflict,
			Detail: "backup already running",
		},
		RunningJobID: "01HABC",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, nested := m["Problem"]; nested {
		t.Errorf("Problem key nested; expected base fields flattened: got %v", m)
	}
	if m["title"] == nil || m["status"] == nil || m["detail"] == nil {
		t.Errorf("missing flattened base fields: got %v", m)
	}
	if m["running_job_id"] != "01HABC" {
		t.Errorf("running_job_id = %v, want 01HABC", m["running_job_id"])
	}
}
