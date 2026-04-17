package job

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// cancelHandler serves the POST /cancel route and optionally returns a 404
// for the "truly unknown" test.
type cancelHandler struct {
	returnJob  apiclient.BackupJob
	returnCode int   // defaults to 200
	calls      int32 // atomic
}

func (h *cancelHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/cancel") {
		http.NotFound(w, r)
		return
	}
	atomic.AddInt32(&h.calls, 1)
	code := h.returnCode
	if code == 0 {
		code = http.StatusOK
	}
	if code >= 400 {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404,"detail":"job not found"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(h.returnJob)
}

func newCancelServer(t *testing.T, h *cancelHandler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// ---------------------------------------------------------------------------

func TestJobCancel_Running_PrintsHint(t *testing.T) {
	f := newJobTestFixture(t)
	cmdutil.Flags.Output = "table"

	h := &cancelHandler{returnJob: apiclient.BackupJob{
		ID: "01HAJOB00000000000000000J", Kind: "backup", RepoID: "daily-s3", Status: "interrupted",
	}}
	srv := newCancelServer(t, h)
	withJobClient(t, srv.URL)

	if err := runCancel(cancelCmd, []string{"fast-meta", "01HAJOB00000000000000000J"}); err != nil {
		t.Fatalf("runCancel err: %v", err)
	}
	out := f.stdout.String()
	if !strings.Contains(out, "Cancel requested for job 01HAJOB00000000000000000J") {
		t.Errorf("expected cancel banner; got %q", out)
	}
	if !strings.Contains(out, "Poll: dfsctl store metadata fast-meta backup job show 01HAJOB00000000000000000J") {
		t.Errorf("expected poll hint; got %q", out)
	}
}

// TestJobCancel_Terminal_IdempotentPrintsHint — D-45: cancel on a terminal
// job is a no-op that returns 200 OK + the unchanged job; CLI still prints
// the same next-step hint so operators in a Ctrl-C race don't see a
// misleading error.
func TestJobCancel_Terminal_IdempotentPrintsHint(t *testing.T) {
	f := newJobTestFixture(t)
	cmdutil.Flags.Output = "table"

	h := &cancelHandler{returnJob: apiclient.BackupJob{
		ID: "01HAJOB00000000000000000J", Kind: "backup", RepoID: "daily-s3", Status: "succeeded",
	}}
	srv := newCancelServer(t, h)
	withJobClient(t, srv.URL)

	if err := runCancel(cancelCmd, []string{"fast-meta", "01HAJOB00000000000000000J"}); err != nil {
		t.Fatalf("runCancel err: %v", err)
	}
	if atomic.LoadInt32(&h.calls) != 1 {
		t.Errorf("expected 1 cancel POST, got %d", h.calls)
	}
	out := f.stdout.String()
	if !strings.Contains(out, "Cancel requested") {
		t.Errorf("expected idempotent banner; got %q", out)
	}
	if !strings.Contains(out, "Poll:") {
		t.Errorf("expected poll hint; got %q", out)
	}
}

func TestJobCancel_NotFound_ReturnsError(t *testing.T) {
	f := newJobTestFixture(t)
	cmdutil.Flags.Output = "table"

	h := &cancelHandler{returnCode: http.StatusNotFound}
	srv := newCancelServer(t, h)
	withJobClient(t, srv.URL)

	err := runCancel(cancelCmd, []string{"fast-meta", "01HAJOBGHOST000000000000000"})
	if err == nil {
		t.Fatal("expected non-nil error on 404")
	}
	if !strings.Contains(err.Error(), "failed to cancel job") {
		t.Errorf("expected wrapped error message, got %v", err)
	}
	// Success hint MUST NOT appear on stdout when the call failed.
	if strings.Contains(f.stdout.String(), "Cancel requested") {
		t.Errorf("success hint should not render on 404; got %q", f.stdout.String())
	}
}

func TestJobCancel_JSONMode_EmitsRecord(t *testing.T) {
	f := newJobTestFixture(t)
	cmdutil.Flags.Output = "json"

	h := &cancelHandler{returnJob: apiclient.BackupJob{
		ID: "01HAJOB00000000000000000J", Kind: "backup", RepoID: "daily-s3", Status: "interrupted",
	}}
	srv := newCancelServer(t, h)
	withJobClient(t, srv.URL)

	if err := runCancel(cancelCmd, []string{"fast-meta", "01HAJOB00000000000000000J"}); err != nil {
		t.Fatalf("runCancel err: %v", err)
	}
	out := f.stdout.String()
	if !strings.Contains(out, `"id": "01HAJOB00000000000000000J"`) {
		t.Errorf("expected JSON body with job id; got %q", out)
	}
	if strings.Contains(out, "Cancel requested") {
		t.Errorf("JSON mode should not print the banner; got %q", out)
	}
}
