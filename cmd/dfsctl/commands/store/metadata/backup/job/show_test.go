package job

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

func newShowServer(t *testing.T, job apiclient.BackupJob) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(job)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestJobShow_Running_IncludesProgressBar(t *testing.T) {
	f := newJobTestFixture(t)
	cmdutil.Flags.Output = "table"

	started := time.Now().Add(-2 * time.Minute)
	job := apiclient.BackupJob{
		ID:        "01HABCDEFGHJKMNPQRSTUVWXYZ",
		Kind:      "backup",
		RepoID:    "daily-s3",
		Status:    "running",
		Progress:  60,
		StartedAt: &started,
	}
	srv := newShowServer(t, job)
	withJobClient(t, srv.URL)

	if err := runShow(showCmd, []string{"fast-meta", job.ID}); err != nil {
		t.Fatalf("runShow err: %v", err)
	}
	out := f.stdout.String()
	if !strings.Contains(out, "60%") {
		t.Errorf("expected 60%% progress in output; got %q", out)
	}
	if !strings.Contains(out, "\u2593") || !strings.Contains(out, "\u2591") {
		t.Errorf("expected progress bar cells in output; got %q", out)
	}
}

func TestJobShow_Terminal_OmitsProgressBar(t *testing.T) {
	f := newJobTestFixture(t)
	cmdutil.Flags.Output = "table"

	started := time.Now().Add(-10 * time.Minute)
	finished := time.Now().Add(-5 * time.Minute)
	job := apiclient.BackupJob{
		ID:         "01HABCDEFGHJKMNPQRSTUVWXYZ",
		Kind:       "backup",
		RepoID:     "daily-s3",
		Status:     "succeeded",
		Progress:   100,
		StartedAt:  &started,
		FinishedAt: &finished,
	}
	srv := newShowServer(t, job)
	withJobClient(t, srv.URL)

	if err := runShow(showCmd, []string{"fast-meta", job.ID}); err != nil {
		t.Fatalf("runShow err: %v", err)
	}
	out := f.stdout.String()
	if strings.Contains(out, "Progress") {
		// Row label "Progress" should be absent when status is not running.
		t.Errorf("progress row should be hidden on terminal state; got %q", out)
	}
	if strings.Contains(out, "\u2593") {
		t.Errorf("progress bar should not render on terminal state; got %q", out)
	}
	if !strings.Contains(out, "succeeded") {
		t.Errorf("expected Status row; got %q", out)
	}
}

func TestJobShow_WithError_RendersErrorRow(t *testing.T) {
	f := newJobTestFixture(t)
	cmdutil.Flags.Output = "table"

	started := time.Now().Add(-5 * time.Minute)
	finished := time.Now().Add(-4 * time.Minute)
	job := apiclient.BackupJob{
		ID:         "01HABCDEFGHJKMNPQRSTUVWXYZ",
		Kind:       "backup",
		RepoID:     "daily-s3",
		Status:     "failed",
		StartedAt:  &started,
		FinishedAt: &finished,
		Error:      "boom: destination unreachable",
	}
	srv := newShowServer(t, job)
	withJobClient(t, srv.URL)

	if err := runShow(showCmd, []string{"fast-meta", job.ID}); err != nil {
		t.Fatalf("runShow err: %v", err)
	}
	out := f.stdout.String()
	if !strings.Contains(out, "Error") || !strings.Contains(out, "boom") {
		t.Errorf("expected Error row with 'boom' text; got %q", out)
	}
}

func TestJobShow_JSONMode_PassesThrough(t *testing.T) {
	f := newJobTestFixture(t)
	cmdutil.Flags.Output = "json"

	started := time.Now().Add(-30 * time.Second)
	job := apiclient.BackupJob{
		ID: "01HABCDEFGHJKMNPQRSTUVWXYZ", Kind: "backup", RepoID: "daily-s3",
		Status: "running", Progress: 10, StartedAt: &started,
	}
	srv := newShowServer(t, job)
	withJobClient(t, srv.URL)

	if err := runShow(showCmd, []string{"fast-meta", job.ID}); err != nil {
		t.Fatalf("runShow err: %v", err)
	}
	// JSON output must NOT contain the derived Duration field or the bar
	// characters — only the flat BackupJob shape.
	out := f.stdout.String()
	if strings.Contains(out, "\u2593") {
		t.Errorf("JSON must not include progress bar cells; got %q", out)
	}
	if !strings.Contains(out, `"status": "running"`) {
		t.Errorf("expected status field in JSON; got %q", out)
	}
}
