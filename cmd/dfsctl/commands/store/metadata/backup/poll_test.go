package backup

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// fakePoller drives WaitForJob deterministically from tests. It exposes
// the same two methods WaitForJob depends on: GetBackupJob + CancelBackupJob.
type fakePoller struct {
	mu             sync.Mutex
	getStatuses    []string // consumed in order; last status is "sticky"
	getErrs        []error  // aligned with getStatuses
	getCalls       int
	cancelErr      error
	cancelCalled   int32 // atomic
	cancelledJob   *apiclient.BackupJob
	cancelSwitches bool // when true, subsequent GetBackupJob returns "interrupted"
}

func (f *fakePoller) GetBackupJob(_, jobID string) (*apiclient.BackupJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.getCalls
	f.getCalls++

	// When cancel flipped the track, force interrupted terminal next time.
	if f.cancelSwitches && atomic.LoadInt32(&f.cancelCalled) > 0 {
		return &apiclient.BackupJob{ID: jobID, Status: "interrupted"}, nil
	}

	if idx >= len(f.getStatuses) {
		// Sticky last status.
		idx = len(f.getStatuses) - 1
	}
	status := f.getStatuses[idx]
	var err error
	if idx < len(f.getErrs) {
		err = f.getErrs[idx]
	}
	if err != nil {
		return nil, err
	}
	started := time.Now().Add(-30 * time.Second)
	return &apiclient.BackupJob{ID: jobID, Status: status, StartedAt: &started}, nil
}

func (f *fakePoller) CancelBackupJob(_, jobID string) (*apiclient.BackupJob, error) {
	atomic.AddInt32(&f.cancelCalled, 1)
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelledJob = &apiclient.BackupJob{ID: jobID, Status: "interrupted"}
	return f.cancelledJob, nil
}

// ---------------------------------------------------------------------------
// Test plumbing — every helper restores global state via t.Cleanup so tests
// stay independent.
// ---------------------------------------------------------------------------

func withFastPoll(t *testing.T) {
	t.Helper()
	prev := pollInterval
	pollInterval = 5 * time.Millisecond
	t.Cleanup(func() { pollInterval = prev })
}

func withSpinnerOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := spinnerOut
	spinnerOut = buf
	t.Cleanup(func() { spinnerOut = prev })
	return buf
}

func withStderrOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := stderrOut
	stderrOut = buf
	t.Cleanup(func() { stderrOut = prev })
	return buf
}

// withInterrupt returns a trigger func that simulates one Ctrl-C. On every
// re-arm inside WaitForJob the factory produces a *fresh* ctx so the first
// trigger doesn't bleed into the re-armed ctx.
func withInterrupt(t *testing.T) func() {
	t.Helper()
	prev := notifyInterrupt

	var (
		mu      sync.Mutex
		current context.CancelFunc
	)
	notifyInterrupt = func(ctx context.Context) (context.Context, context.CancelFunc) {
		c, cancel := context.WithCancel(ctx)
		mu.Lock()
		current = cancel
		mu.Unlock()
		return c, cancel
	}
	t.Cleanup(func() { notifyInterrupt = prev })

	return func() {
		mu.Lock()
		fn := current
		// Single-shot: clear so re-armed ctxs are NOT cancelled.
		current = nil
		mu.Unlock()
		if fn != nil {
			fn()
		}
	}
}

func withInterruptHandler(t *testing.T, response string) {
	t.Helper()
	prev := interruptHandler
	interruptHandler = func(_, _ string) string { return response }
	t.Cleanup(func() { interruptHandler = prev })
}

// ---------------------------------------------------------------------------
// WaitForJob tests
// ---------------------------------------------------------------------------

func TestWaitForJob_ReachesTerminal(t *testing.T) {
	withFastPoll(t)
	buf := withSpinnerOut(t)

	poller := &fakePoller{getStatuses: []string{"running", "running", "succeeded"}}
	job, err := WaitForJob(context.Background(), poller, WaitOptions{
		StoreName: "fast-meta", JobID: "01HABCDEFGHJKMNPQRST",
		Format: output.FormatTable,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if job.Status != "succeeded" {
		t.Fatalf("want succeeded, got %q", job.Status)
	}
	// Spinner should have emitted at least one transition line.
	if !strings.Contains(buf.String(), "Status:") {
		t.Errorf("spinner did not render status transition; got %q", buf.String())
	}
}

func TestWaitForJob_Timeout_ExitsWithSpecificError(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	stderr := withStderrOut(t)

	poller := &fakePoller{getStatuses: []string{"running"}}
	_, err := WaitForJob(context.Background(), poller, WaitOptions{
		StoreName: "s", JobID: "j", Format: output.FormatTable,
		Timeout: 30 * time.Millisecond,
	})
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("want ErrPollTimeout, got %v", err)
	}
	if !strings.Contains(stderr.String(), "Timeout") {
		t.Errorf("expected timeout hint on stderr, got %q", stderr.String())
	}
}

func TestWaitForJob_CtrlC_Detach_PrintsDetachMessage(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	stderr := withStderrOut(t)
	trigger := withInterrupt(t)
	withInterruptHandler(t, "d")

	poller := &fakePoller{getStatuses: []string{"running"}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(15 * time.Millisecond)
		trigger()
	}()

	_, err := WaitForJob(context.Background(), poller, WaitOptions{
		StoreName: "fast-meta", JobID: "jid",
		Format: output.FormatTable,
	})
	<-done
	if !errors.Is(err, ErrPollDetached) {
		t.Fatalf("want ErrPollDetached, got %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Detached") || !strings.Contains(out, "Poll:") {
		t.Errorf("expected detach hint on stderr; got %q", out)
	}
}

func TestWaitForJob_CtrlC_Cancel_CallsAPI(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	withStderrOut(t)
	trigger := withInterrupt(t)
	withInterruptHandler(t, "c")

	poller := &fakePoller{
		getStatuses:    []string{"running"},
		cancelSwitches: true,
	}

	go func() {
		time.Sleep(15 * time.Millisecond)
		trigger()
	}()

	job, err := WaitForJob(context.Background(), poller, WaitOptions{
		StoreName: "s", JobID: "j", Format: output.FormatTable,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if atomic.LoadInt32(&poller.cancelCalled) == 0 {
		t.Fatalf("expected CancelBackupJob to be called")
	}
	if job.Status != "interrupted" {
		t.Errorf("expected interrupted terminal, got %q", job.Status)
	}
}

func TestWaitForJob_CtrlC_Continue_ResumesSpinner(t *testing.T) {
	withFastPoll(t)
	withSpinnerOut(t)
	withStderrOut(t)
	trigger := withInterrupt(t)
	withInterruptHandler(t, "C")

	poller := &fakePoller{getStatuses: []string{"running", "running", "running", "succeeded"}}

	go func() {
		time.Sleep(8 * time.Millisecond)
		trigger()
	}()

	job, err := WaitForJob(context.Background(), poller, WaitOptions{
		StoreName: "s", JobID: "j", Format: output.FormatTable,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if atomic.LoadInt32(&poller.cancelCalled) != 0 {
		t.Errorf("expected no cancel call on [C]ontinue, got %d", poller.cancelCalled)
	}
	if job.Status != "succeeded" {
		t.Errorf("want succeeded, got %q", job.Status)
	}
}

func TestWaitForJob_JSONMode_EmitsNothingDuringRun(t *testing.T) {
	withFastPoll(t)
	buf := withSpinnerOut(t)

	poller := &fakePoller{getStatuses: []string{"running", "running", "succeeded"}}
	_, err := WaitForJob(context.Background(), poller, WaitOptions{
		StoreName: "s", JobID: "j", Format: output.FormatJSON,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("JSON mode should emit nothing to spinnerOut; got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Format helpers
// ---------------------------------------------------------------------------
//
// ShortULID / TimeAgoSince / RenderProgressBar live in
// internal/cli/backupfmt now and are tested there. humanSize is still
// package-local.

func TestHumanSize_RendersMB(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{512, "512B"},
		{1024, "1.0KB"},
		{1024 * 1024, "1.0MB"},
		{int64(1.5 * 1024 * 1024), "1.5MB"},
	}
	for _, tc := range cases {
		got := humanSize(tc.bytes)
		if got != tc.want {
			t.Errorf("bytes=%d: want %q, got %q", tc.bytes, tc.want, got)
		}
	}
}

func TestSuccessExitCode_Terminal(t *testing.T) {
	cases := []struct {
		status string
		want   int
	}{
		{"succeeded", 0},
		{"failed", 1},
		{"interrupted", 1},
	}
	for _, tc := range cases {
		if got := SuccessExitCode(tc.status); got != tc.want {
			t.Errorf("status=%s: want %d, got %d", tc.status, tc.want, got)
		}
	}
}
