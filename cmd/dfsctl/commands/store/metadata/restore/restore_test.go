package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// -----------------------------------------------------------------------------
// Test fixtures
// -----------------------------------------------------------------------------

// restoreTestFixture bundles the swappable globals mutated by restore.go unit
// tests. Each test grabs a fresh fixture via newRestoreTestFixture(t) which
// installs t.Cleanup restorers.
type restoreTestFixture struct {
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	exit   *int32 // -1 = exitFunc not invoked
}

func newRestoreTestFixture(t *testing.T) *restoreTestFixture {
	t.Helper()
	f := &restoreTestFixture{
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		exit:   new(int32),
	}
	*f.exit = -1

	prevStdout := stdoutOut
	prevStderr := stderrOut
	prevExit := exitFunc
	prevOutput := cmdutil.Flags.Output

	stdoutOut = f.stdout
	stderrOut = f.stderr
	exitFunc = func(code int) { atomic.StoreInt32(f.exit, int32(code)) }

	t.Cleanup(func() {
		stdoutOut = prevStdout
		stderrOut = prevStderr
		exitFunc = prevExit
		cmdutil.Flags.Output = prevOutput
	})
	return f
}

// setRestoreFlags replaces the package-level flag vars and restores them on
// cleanup. Tests call this rather than going through pflag parsing.
func setRestoreFlags(t *testing.T, fromID string, yes, force, dryRun, wait, async bool, timeout time.Duration) {
	t.Helper()
	o1, o2, o3, o4, o5, o6, o7 := restoreFromID, restoreYes, restoreForce, restoreDryRun, restoreWait, restoreAsync, restoreTimeout
	restoreFromID, restoreYes, restoreForce, restoreDryRun, restoreWait, restoreAsync, restoreTimeout = fromID, yes, force, dryRun, wait, async, timeout
	t.Cleanup(func() {
		restoreFromID, restoreYes, restoreForce, restoreDryRun, restoreWait, restoreAsync, restoreTimeout = o1, o2, o3, o4, o5, o6, o7
	})
}

// withClient swaps clientFactory so RunE gets a client pointed at the given
// httptest server instead of the real credential store.
func withClient(t *testing.T, url string) {
	t.Helper()
	prev := clientFactory
	clientFactory = func() (*apiclient.Client, error) {
		return apiclient.New(url).WithToken("test-token"), nil
	}
	t.Cleanup(func() { clientFactory = prev })
}

// withConfirmFunc swaps the confirmation prompt with a deterministic stub.
func withConfirmFunc(t *testing.T, answer bool, err error) *int32 {
	t.Helper()
	calls := new(int32)
	prev := confirmFunc
	confirmFunc = func(label string, force bool) (bool, error) {
		atomic.AddInt32(calls, 1)
		if force {
			return true, nil
		}
		return answer, err
	}
	t.Cleanup(func() { confirmFunc = prev })
	return calls
}

// withWaitFn replaces waitFn so tests can simulate detach / timeout / error
// returns from WaitForJob without driving the real backup-package poll loop
// (which would require swapping package-private globals we can't reach).
func withWaitFn(t *testing.T, fn func(ctx context.Context, client *apiclient.Client, opts backup.WaitOptions) (*apiclient.BackupJob, error)) {
	t.Helper()
	prev := waitFn
	waitFn = fn
	t.Cleanup(func() { waitFn = prev })
}

// -----------------------------------------------------------------------------
// Stub server — records which endpoints were hit so tests can assert that
// dry-run does NOT call /restore and vice versa.
// -----------------------------------------------------------------------------

type restoreStubHandler struct {
	startCalls   int32
	dryRunCalls  int32
	getJobCalls  int32
	startResp    *apiclient.BackupJob
	startErrBody string
	startStatus  int
	dryRunResp   apiclient.DryRunResult
	jobStatuses  []string
}

func (h *restoreStubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/restore/dry-run"):
		atomic.AddInt32(&h.dryRunCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(h.dryRunResp)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/restore"):
		atomic.AddInt32(&h.startCalls, 1)
		if h.startStatus >= 400 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(h.startStatus)
			_, _ = w.Write([]byte(h.startErrBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(h.startResp)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/backup-jobs/"):
		idx := int(atomic.AddInt32(&h.getJobCalls, 1)) - 1
		if idx >= len(h.jobStatuses) {
			idx = len(h.jobStatuses) - 1
		}
		status := h.jobStatuses[idx]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(apiclient.BackupJob{ID: h.startResp.ID, Status: status})
	default:
		http.NotFound(w, r)
	}
}

func newStubServer(t *testing.T, h *restoreStubHandler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// -----------------------------------------------------------------------------
// Cobra dispatch helper — runRestore needs a *cobra.Command with a context;
// pass a no-op one so cmd.Context() returns background.
// -----------------------------------------------------------------------------

// runWithCtx calls runRestore(Cmd, args) with a pre-seeded background ctx —
// mirrors what Cobra does in production but without invoking cobra.Execute.
func runWithCtx(t *testing.T, args []string) error {
	t.Helper()
	Cmd.SetContext(context.Background())
	return runRestore(Cmd, args)
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

func TestRestore_InvalidULID_ExitsBeforeAPI(t *testing.T) {
	newRestoreTestFixture(t)
	setRestoreFlags(t, "short", true, false, false, true, false, 0)

	// Install a stub server that would blow up if we ever hit it.
	h := &restoreStubHandler{}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)

	err := runWithCtx(t, []string{"fast-meta"})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "26-character ULID") {
		t.Errorf("error should mention 26-character ULID; got %v", err)
	}
	if atomic.LoadInt32(&h.startCalls) != 0 || atomic.LoadInt32(&h.dryRunCalls) != 0 {
		t.Errorf("ULID validation must reject BEFORE any HTTP call (got start=%d dryrun=%d)",
			h.startCalls, h.dryRunCalls)
	}
}

func TestRestore_Yes_SkipsConfirmation(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", true, false, false, true, true /*async*/, 0)
	calls := withConfirmFunc(t, false, nil)

	h := &restoreStubHandler{
		startResp:   &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending"},
		startStatus: http.StatusAccepted,
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("runRestore returned err: %v", err)
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Errorf("confirmFunc must NOT be invoked when --yes is set; got %d calls", *calls)
	}
	if atomic.LoadInt32(&h.startCalls) != 1 {
		t.Errorf("expected exactly one StartRestore call; got %d", h.startCalls)
	}
	if !strings.Contains(f.stdout.String(), "01HAJOB00000000000000000J") {
		t.Errorf("expected job ID on stdout; got %q", f.stdout.String())
	}
}

func TestRestore_NoPromptDeclines_Aborts(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", false, false, false, true, false, 0)
	withConfirmFunc(t, false /*decline*/, nil)

	h := &restoreStubHandler{}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("decline should return nil, got %v", err)
	}
	if atomic.LoadInt32(&h.startCalls) != 0 {
		t.Errorf("StartRestore must NOT be called when user declines; got %d", h.startCalls)
	}
	if !strings.Contains(f.stdout.String(), "Aborted.") {
		t.Errorf("expected Aborted. on stdout; got %q", f.stdout.String())
	}
}

func TestRestore_SharesEnabled409_RendersHint(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", true, false, false, true, false, 0)

	h := &restoreStubHandler{
		startStatus:  http.StatusConflict,
		startErrBody: `{"title":"Conflict","status":409,"detail":"shares still enabled","enabled_shares":["/a","/b"]}`,
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)

	err := runWithCtx(t, []string{"fast-meta"})
	if err == nil {
		t.Fatal("expected non-nil error on 409")
	}
	stderrStr := f.stderr.String()
	if !strings.Contains(stderrStr, "Cannot restore: 2 share(s) enabled") {
		t.Errorf("expected D-29 header on stderr; got %q", stderrStr)
	}
	if !strings.Contains(stderrStr, "dfsctl share '/a' disable") ||
		!strings.Contains(stderrStr, "dfsctl share '/b' disable") {
		t.Errorf("expected per-share disable commands on stderr; got %q", stderrStr)
	}
}

func TestRestore_DryRun_CallsServerEndpoint(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", false, false, true /*dry-run*/, true, false, 0)

	h := &restoreStubHandler{
		dryRunResp: apiclient.DryRunResult{
			Record: &apiclient.BackupRecord{
				ID:        "01HABCDEFGHJKMNPQRSTUVWXY1",
				CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				SizeBytes: 2048,
			},
			ManifestValid: true,
			EnabledShares: []string{"/a"},
		},
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("dry-run returned err: %v", err)
	}
	if atomic.LoadInt32(&h.dryRunCalls) != 1 {
		t.Errorf("expected exactly one RestoreDryRun call; got %d", h.dryRunCalls)
	}
	if atomic.LoadInt32(&h.startCalls) != 0 {
		t.Errorf("dry-run must NOT invoke StartRestore; got %d", h.startCalls)
	}
	stdout := f.stdout.String()
	for _, want := range []string{
		"Dry run:",
		"Target store:",
		"Selected record:",
		"01HABCDEFGHJKMNPQRSTUVWXY1",
		"Manifest:         valid",
		"dfsctl share '/a' disable",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected stdout to contain %q; got %q", want, stdout)
		}
	}
}

func TestRestore_DryRun_ManifestInvalid_RendersWarning(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", false, false, true, true, false, 0)

	h := &restoreStubHandler{
		dryRunResp: apiclient.DryRunResult{
			Record:        &apiclient.BackupRecord{ID: "01HABCDEFGHJKMNPQRSTUVWXY1"},
			ManifestValid: false,
		},
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("dry-run returned err: %v", err)
	}
	if !strings.Contains(f.stdout.String(), "Manifest:         INVALID") {
		t.Errorf("expected INVALID manifest warning; got %q", f.stdout.String())
	}
}

func TestRestore_DryRun_SkipsSharesEnabledGate(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", false, false, true, true, false, 0)

	h := &restoreStubHandler{
		dryRunResp: apiclient.DryRunResult{
			Record:        &apiclient.BackupRecord{ID: "01HABCDEFGHJKMNPQRSTUVWXY1"},
			ManifestValid: true,
			EnabledShares: []string{"/still-on"},
		},
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("D-31 dry-run MUST NOT fail on enabled shares; got err=%v", err)
	}
	if !strings.Contains(f.stdout.String(), "/still-on") {
		t.Errorf("expected enabled share listed in note; got %q", f.stdout.String())
	}
	// No stderr precondition error.
	if strings.Contains(f.stderr.String(), "Cannot restore") {
		t.Errorf("dry-run must NOT print precondition error; got %q", f.stderr.String())
	}
}

func TestRestore_Async_EmitsJob(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", true, false, false, true, true /*async*/, 0)

	h := &restoreStubHandler{
		startResp:   &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending"},
		startStatus: http.StatusAccepted,
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("async: %v", err)
	}
	if !strings.Contains(f.stdout.String(), "01HAJOB00000000000000000J") {
		t.Errorf("expected job ID on stdout; got %q", f.stdout.String())
	}
	wantHint := "Poll: dfsctl store metadata fast-meta backup job show 01HAJOB00000000000000000J"
	if !strings.Contains(f.stderr.String(), wantHint) {
		t.Errorf("expected poll hint on stderr; got %q", f.stderr.String())
	}
	if atomic.LoadInt32(f.exit) != -1 {
		t.Errorf("async: exitFunc must NOT be invoked; got %d", *f.exit)
	}
}

func TestRestore_WaitSucceeds_PrintsReEnableHint(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", true, false, false, true, false, 0)

	h := &restoreStubHandler{
		startResp:   &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending"},
		startStatus: http.StatusAccepted,
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)
	withWaitFn(t, func(ctx context.Context, client *apiclient.Client, opts backup.WaitOptions) (*apiclient.BackupJob, error) {
		return &apiclient.BackupJob{ID: opts.JobID, Status: "succeeded"}, nil
	})

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("wait-succeeds: %v", err)
	}
	stdout := f.stdout.String()
	for _, want := range []string{
		"\u2713 Restore succeeded",
		"Shares remain disabled. Re-enable with:",
		"dfsctl share <name> enable",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected stdout to contain %q; got %q", want, stdout)
		}
	}
	if got := atomic.LoadInt32(f.exit); got != 0 {
		t.Errorf("want exit 0 on success, got %d", got)
	}
}

func TestRestore_WaitFailed_ExitsNonZero(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", true, false, false, true, false, 0)

	h := &restoreStubHandler{
		startResp:   &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending"},
		startStatus: http.StatusAccepted,
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)
	withWaitFn(t, func(ctx context.Context, client *apiclient.Client, opts backup.WaitOptions) (*apiclient.BackupJob, error) {
		return &apiclient.BackupJob{ID: opts.JobID, Status: "failed"}, nil
	})

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("wait-failed: runRestore must return nil (exit code signals failure), got %v", err)
	}
	if got := atomic.LoadInt32(f.exit); got != 1 {
		t.Errorf("want exit 1 on failed terminal, got %d", got)
	}
}

func TestRestore_Timeout_ExitsTwo(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", true, false, false, true, false, 100*time.Millisecond)

	h := &restoreStubHandler{
		startResp:   &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending"},
		startStatus: http.StatusAccepted,
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)
	withWaitFn(t, func(ctx context.Context, client *apiclient.Client, opts backup.WaitOptions) (*apiclient.BackupJob, error) {
		return nil, backup.ErrPollTimeout
	})

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("runRestore returned err on timeout, got %v", err)
	}
	if got := atomic.LoadInt32(f.exit); got != 2 {
		t.Errorf("want exit 2 on timeout (ErrPollTimeout), got %d", got)
	}
}

// TestRestore_CtrlCDetach_ExitsZero — regression guard aligned with Plan 05
// Warning 10. Detach must NOT call exitFunc (cobra surfaces nil RunE return
// as exit 0); stdout must stay clean so piped -o json scripts don't see a
// half-written record.
func TestRestore_CtrlCDetach_ExitsZero(t *testing.T) {
	f := newRestoreTestFixture(t)
	setRestoreFlags(t, "", true, false, false, true, false, 0)

	h := &restoreStubHandler{
		startResp:   &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending"},
		startStatus: http.StatusAccepted,
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)
	withWaitFn(t, func(ctx context.Context, client *apiclient.Client, opts backup.WaitOptions) (*apiclient.BackupJob, error) {
		return nil, backup.ErrPollDetached
	})

	if err := runWithCtx(t, []string{"fast-meta"}); err != nil {
		t.Fatalf("detach: runRestore must return nil, got %v", err)
	}
	if got := atomic.LoadInt32(f.exit); got != -1 {
		t.Errorf("detach must NOT call exitFunc; got exit=%d (Warning 10 regression)", got)
	}
	if strings.Contains(f.stdout.String(), "Restore succeeded") {
		t.Errorf("stdout must stay clean on detach; got %q", f.stdout.String())
	}
}

// TestRestore_WaitGenericError_BubblesUp guards that non-sentinel errors
// from WaitForJob (e.g., transport failures) propagate as plain Cobra RunE
// errors — NOT swallowed or masked as detach.
func TestRestore_WaitGenericError_BubblesUp(t *testing.T) {
	newRestoreTestFixture(t)
	setRestoreFlags(t, "", true, false, false, true, false, 0)

	h := &restoreStubHandler{
		startResp:   &apiclient.BackupJob{ID: "01HAJOB00000000000000000J", Status: "pending"},
		startStatus: http.StatusAccepted,
	}
	srv := newStubServer(t, h)
	withClient(t, srv.URL)
	bogus := errors.New("bogus transport")
	withWaitFn(t, func(ctx context.Context, client *apiclient.Client, opts backup.WaitOptions) (*apiclient.BackupJob, error) {
		return nil, bogus
	})

	err := runWithCtx(t, []string{"fast-meta"})
	if !errors.Is(err, bogus) {
		t.Errorf("expected runRestore to bubble up the transport error; got %v", err)
	}
}
