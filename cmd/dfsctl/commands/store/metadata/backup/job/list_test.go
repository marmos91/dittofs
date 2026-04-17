package job

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// jobTestFixture redirects stdout + stderr and restores them on cleanup.
type jobTestFixture struct {
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newJobTestFixture(t *testing.T) *jobTestFixture {
	t.Helper()
	f := &jobTestFixture{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	prevStdout, prevStderr, prevOutput := stdoutOut, stderrOut, cmdutil.Flags.Output
	stdoutOut = f.stdout
	stderrOut = f.stderr
	t.Cleanup(func() {
		stdoutOut = prevStdout
		stderrOut = prevStderr
		cmdutil.Flags.Output = prevOutput
	})
	return f
}

func withJobClient(t *testing.T, url string) {
	t.Helper()
	prev := clientFactory
	clientFactory = func() (*apiclient.Client, error) {
		return apiclient.New(url).WithToken("test"), nil
	}
	t.Cleanup(func() { clientFactory = prev })
}

func withListFlags(t *testing.T, status, kind, repo string, limit int) {
	t.Helper()
	oS, oK, oR, oL := listStatus, listKind, listRepo, listLimit
	listStatus, listKind, listRepo, listLimit = status, kind, repo, limit
	t.Cleanup(func() {
		listStatus, listKind, listRepo, listLimit = oS, oK, oR, oL
	})
}

// listHandler captures the query parameters of the most recent GET so tests
// can assert what the apiclient serialised.
type listHandler struct {
	jobs       []apiclient.BackupJob
	lastStatus string
	lastKind   string
	lastRepo   string
	lastLimit  string
	callCount  int
}

func (h *listHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	h.lastStatus = r.URL.Query().Get("status")
	h.lastKind = r.URL.Query().Get("kind")
	h.lastRepo = r.URL.Query().Get("repo")
	h.lastLimit = r.URL.Query().Get("limit")
	h.callCount++
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(h.jobs)
}

func newListServer(t *testing.T, h *listHandler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// ---------------------------------------------------------------------------

func TestJobList_FilterValidation_StatusRejected(t *testing.T) {
	newJobTestFixture(t)
	withListFlags(t, "bogus", "", "", 0)
	// No server → if client is called, we get a connection error. The
	// validator must short-circuit with a clear message BEFORE that.

	err := runList(listCmd, []string{"fast-meta"})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid status") {
		t.Errorf("expected 'invalid status' in error, got %v", err)
	}
}

func TestJobList_FilterValidation_KindRejected(t *testing.T) {
	newJobTestFixture(t)
	withListFlags(t, "", "bogus", "", 0)

	err := runList(listCmd, []string{"fast-meta"})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid kind") {
		t.Errorf("expected 'invalid kind' in error, got %v", err)
	}
}

func TestJobList_Limit_DefaultsAndPassThrough(t *testing.T) {
	newJobTestFixture(t)

	h := &listHandler{jobs: []apiclient.BackupJob{}}
	srv := newListServer(t, h)
	withJobClient(t, srv.URL)

	// Default: limit=0 means "don't pass a ?limit= query param".
	withListFlags(t, "", "", "", 0)
	if err := runList(listCmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runList err: %v", err)
	}
	if h.lastLimit != "" {
		t.Errorf("default limit should omit ?limit= param; got %q", h.lastLimit)
	}

	// Explicit --limit 150 must pass through.
	withListFlags(t, "", "", "", 150)
	if err := runList(listCmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runList err (with limit): %v", err)
	}
	if h.lastLimit != "150" {
		t.Errorf("explicit --limit 150 not passed through; got %q", h.lastLimit)
	}
	// Sanity: integer conversion works both ways.
	if n, _ := strconv.Atoi(h.lastLimit); n != 150 {
		t.Errorf("limit roundtrip failed, got %d", n)
	}
}

func TestJobList_Table_RendersColumns(t *testing.T) {
	f := newJobTestFixture(t)

	started := time.Now().Add(-2 * time.Minute)
	finished := time.Now().Add(-30 * time.Second)
	h := &listHandler{
		jobs: []apiclient.BackupJob{
			{
				ID: "01HABCDEFGHJKMNPQRSTUVWXYZ", Kind: "backup", RepoID: "daily-s3",
				Status: "succeeded", StartedAt: &started, FinishedAt: &finished, Progress: 100,
			},
			{
				ID: "01HWXYZ0000000000000000000", Kind: "restore", RepoID: "daily-s3",
				Status: "running", StartedAt: &started, Progress: 60,
			},
		},
	}
	srv := newListServer(t, h)
	withJobClient(t, srv.URL)
	withListFlags(t, "", "", "", 0)

	if err := runList(listCmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runList err: %v", err)
	}
	out := f.stdout.String()
	for _, col := range []string{"JOB ID", "KIND", "REPO", "STATUS", "STARTED", "DURATION", "PROGRESS"} {
		if !strings.Contains(out, col) {
			t.Errorf("missing column %q in output; got %q", col, out)
		}
	}
	if !strings.Contains(out, "01HABCDE\u2026") {
		t.Errorf("expected short ULID in row; got %q", out)
	}
	if !strings.Contains(out, "60%") {
		t.Errorf("expected 60%% progress cell; got %q", out)
	}
}

func TestJobList_EmptyShowsHint(t *testing.T) {
	f := newJobTestFixture(t)

	h := &listHandler{jobs: []apiclient.BackupJob{}}
	srv := newListServer(t, h)
	withJobClient(t, srv.URL)
	withListFlags(t, "", "", "daily-s3", 0)

	if err := runList(listCmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runList err: %v", err)
	}
	out := f.stdout.String()
	if !strings.Contains(out, "No backup jobs found") {
		t.Errorf("expected empty hint; got %q", out)
	}
	if !strings.Contains(out, "--repo daily-s3") {
		t.Errorf("expected --repo in empty hint; got %q", out)
	}
}

func TestJobList_FilterValidation_NegativeLimitRejected(t *testing.T) {
	newJobTestFixture(t)
	withListFlags(t, "", "", "", -5)

	err := runList(listCmd, []string{"fast-meta"})
	if err == nil {
		t.Fatal("expected negative-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "--limit must be non-negative") {
		t.Errorf("unexpected error text: %v", err)
	}
}
