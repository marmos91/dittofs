package backup

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// runTestFixture wraps the package-level globals touched by run.go so tests
// can mutate them deterministically and rely on t.Cleanup to restore.
type runTestFixture struct {
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	exit   *int32 // captured exit code; -1 = not invoked
}

func newRunTestFixture(t *testing.T) *runTestFixture {
	t.Helper()

	f := &runTestFixture{
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		exit:   new(int32),
	}

	prevStdout := stdoutOut
	prevStderr := stderrOut
	prevExit := exitFunc
	prevOutput := cmdutil.Flags.Output

	stdoutOut = f.stdout
	stderrOut = f.stderr
	*f.exit = -1
	exitFunc = func(code int) { atomic.StoreInt32(f.exit, int32(code)) }

	t.Cleanup(func() {
		stdoutOut = prevStdout
		stderrOut = prevStderr
		exitFunc = prevExit
		cmdutil.Flags.Output = prevOutput
	})
	return f
}

// withClient swaps the package-level clientFactory so RunE gets a client
// pointed at the given httptest server instead of going through the
// credential store.
func withClient(t *testing.T, url string) {
	t.Helper()
	prev := clientFactory
	clientFactory = func() (*apiclient.Client, error) {
		return apiclient.New(url).WithToken("test-token"), nil
	}
	t.Cleanup(func() { clientFactory = prev })
}

// setRunFlags sets the package-level run flags and restores them on cleanup.
func setRunFlags(t *testing.T, repo string, async, wait bool, timeout time.Duration) {
	t.Helper()
	oRepo, oAsync, oWait, oTO := runRepo, runAsync, runWait, runTimeout
	runRepo, runAsync, runWait, runTimeout = repo, async, wait, timeout
	t.Cleanup(func() {
		runRepo, runAsync, runWait, runTimeout = oRepo, oAsync, oWait, oTO
	})
}

// problemResp is a sentinel for the server to return a 409 problem body.
type problemResp struct {
	Status int
	Body   string
}

// triggerHandler serves the trigger POST + job poll GET + cancel POST routes
// for the run.go integration tests. jobStatuses drives the poll timeline;
// once the last status is reached it's sticky.
type triggerHandler struct {
	mu           sync.Mutex
	triggerResp  apiclient.TriggerBackupResponse
	triggerErr   *problemResp
	jobStatuses  []string
	jobCalls     int
	cancelCalled int32
}

func (h *triggerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/backups"):
		if h.triggerErr != nil {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(h.triggerErr.Status)
			_, _ = w.Write([]byte(h.triggerErr.Body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(h.triggerResp)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/backup-jobs/"):
		h.mu.Lock()
		idx := h.jobCalls
		h.jobCalls++
		if idx >= len(h.jobStatuses) {
			idx = len(h.jobStatuses) - 1
		}
		status := h.jobStatuses[idx]
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(apiclient.BackupJob{
			ID: h.triggerResp.Job.ID, Status: status,
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
		atomic.AddInt32(&h.cancelCalled, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(apiclient.BackupJob{ID: h.triggerResp.Job.ID, Status: "interrupted"})
	default:
		http.NotFound(w, r)
	}
}

func newTriggerServer(t *testing.T, h *triggerHandler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// ---------------------------------------------------------------------------

func TestBackupRun_DefaultWait_PollsToTerminal(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	f := newRunTestFixture(t)

	started := time.Now().Add(-10 * time.Second)
	finished := time.Now()
	h := &triggerHandler{
		triggerResp: apiclient.TriggerBackupResponse{
			Record: &apiclient.BackupRecord{ID: "01HAREC00000000000000000R", SizeBytes: 1024 * 1024},
			Job:    &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending", StartedAt: &started, FinishedAt: &finished},
		},
		jobStatuses: []string{"running", "running", "succeeded"},
	}
	srv := newTriggerServer(t, h)
	withClient(t, srv.URL)
	setRunFlags(t, "daily-s3", false, true, 0)

	if err := runRun(Cmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runRun returned err: %v", err)
	}
	if got := atomic.LoadInt32(f.exit); got != 0 {
		t.Errorf("want exit 0, got %d", got)
	}
	if !strings.Contains(f.stdout.String(), "\u2713 Backup completed") {
		t.Errorf("expected success banner on stdout; got %q", f.stdout.String())
	}
	if !strings.Contains(f.stdout.String(), "Record:    01HAREC00000000000000000R") {
		t.Errorf("expected Record ID line; got %q", f.stdout.String())
	}
}

func TestBackupRun_Async_EmitsJobAndHint(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	f := newRunTestFixture(t)

	h := &triggerHandler{
		triggerResp: apiclient.TriggerBackupResponse{
			Record: &apiclient.BackupRecord{ID: "01HAREC00000000000000000R"},
			Job:    &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending"},
		},
	}
	srv := newTriggerServer(t, h)
	withClient(t, srv.URL)
	setRunFlags(t, "daily-s3", true, true, 0)

	if err := runRun(Cmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runRun returned err: %v", err)
	}
	if got := atomic.LoadInt32(f.exit); got != -1 {
		t.Errorf("expected exitFunc NOT called on --async, got exit=%d", got)
	}
	if !strings.Contains(f.stdout.String(), "01HAJOB00000000000000000J") {
		t.Errorf("stdout should render job ID; got %q", f.stdout.String())
	}
	if !strings.Contains(f.stderr.String(), "Poll: dfsctl store metadata fast-meta backup job show 01HAJOB00000000000000000J") {
		t.Errorf("expected poll hint on stderr; got %q", f.stderr.String())
	}
}

func TestBackupRun_AlreadyRunning_SurfaceHint(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	f := newRunTestFixture(t)

	h := &triggerHandler{
		triggerErr: &problemResp{
			Status: http.StatusConflict,
			Body:   `{"title":"Conflict","status":409,"detail":"backup already running","running_job_id":"01HAJOBALREADYRUNNING0001"}`,
		},
	}
	srv := newTriggerServer(t, h)
	withClient(t, srv.URL)
	setRunFlags(t, "daily-s3", false, true, 0)

	err := runRun(Cmd, []string{"fast-meta"})
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error should mention already running, got %v", err)
	}
	if !strings.Contains(f.stderr.String(), "Backup already running: 01HAJOBALREADYRUNNING0001") {
		t.Errorf("expected already-running hint on stderr; got %q", f.stderr.String())
	}
	if !strings.Contains(f.stderr.String(), "Show: dfsctl store metadata fast-meta backup job show") {
		t.Errorf("expected show hint on stderr; got %q", f.stderr.String())
	}
}

func TestBackupRun_TimeoutDetach_ExitsTwo(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	f := newRunTestFixture(t)

	h := &triggerHandler{
		triggerResp: apiclient.TriggerBackupResponse{
			Record: &apiclient.BackupRecord{ID: "rec"},
			Job:    &apiclient.BackupJob{ID: "job", Status: "running"},
		},
		jobStatuses: []string{"running"},
	}
	srv := newTriggerServer(t, h)
	withClient(t, srv.URL)
	setRunFlags(t, "daily-s3", false, true, 30*time.Millisecond)

	if err := runRun(Cmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runRun returned err: %v", err)
	}
	if got := atomic.LoadInt32(f.exit); got != 2 {
		t.Errorf("want exit 2 on timeout, got %d", got)
	}
	if !strings.Contains(f.stderr.String(), "Timeout") {
		t.Errorf("expected timeout hint on stderr; got %q", f.stderr.String())
	}
}

// TestBackupRun_CtrlCDetach_ExitsZero — regression guard for Warning 10 per
// plan 06-05 success criteria. Detach must NOT call os.Exit (cobra surfaces
// a nil RunE return as exit 0); stdout must stay clean so piped `-o json`
// scripts don't see a half-written record.
func TestBackupRun_CtrlCDetach_ExitsZero(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	f := newRunTestFixture(t)
	trigger := withInterrupt(t)
	withInterruptHandler(t, "d")

	h := &triggerHandler{
		triggerResp: apiclient.TriggerBackupResponse{
			Record: &apiclient.BackupRecord{ID: "rec"},
			Job:    &apiclient.BackupJob{ID: "job", Status: "running"},
		},
		jobStatuses: []string{"running"},
	}
	srv := newTriggerServer(t, h)
	withClient(t, srv.URL)
	setRunFlags(t, "daily-s3", false, true, 0)

	go func() {
		time.Sleep(15 * time.Millisecond)
		trigger()
	}()

	if err := runRun(Cmd, []string{"fast-meta"}); err != nil {
		t.Fatalf("runRun returned err: %v", err)
	}
	if got := atomic.LoadInt32(f.exit); got != -1 {
		t.Errorf("detach must NOT call os.Exit; got exit=%d (Warning 10 regression)", got)
	}
	if !strings.Contains(f.stderr.String(), "Detached") {
		t.Errorf("expected Detached hint on stderr; got %q", f.stderr.String())
	}
	if strings.Contains(f.stdout.String(), "Backup completed") {
		t.Errorf("stdout should NOT contain completion banner on detach; got %q", f.stdout.String())
	}
}

func TestParseFormat_BadValueFallsBackToTable(t *testing.T) {
	prev := cmdutil.Flags.Output
	cmdutil.Flags.Output = "xml"
	defer func() { cmdutil.Flags.Output = prev }()

	if got := parseFormat(); got != output.FormatTable {
		t.Errorf("want FormatTable fallback, got %q", got)
	}
}
