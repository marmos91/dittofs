package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// Terminal job statuses per Phase-1 BackupStatus enum. The poll loop stops
// once the server reports any of these (D-06 / D-45 / Phase-4 D-18).
var terminalStatuses = map[string]bool{
	"succeeded":   true,
	"failed":      true,
	"interrupted": true,
}

// Sentinel errors returned by WaitForJob. Callers map these to exit codes
// per D-06 / D-08 / D-11.
var (
	// ErrPollTimeout is returned when WaitOptions.Timeout elapsed before
	// the job reached a terminal state. Callers exit 2 (D-11).
	ErrPollTimeout = errors.New("wait timeout")

	// ErrPollDetached is returned when the user chose [d]etach at the
	// Ctrl-C prompt. Callers exit 0 — the user voluntarily stopped
	// watching, the job is still running server-side (D-08).
	ErrPollDetached = errors.New("detached")
)

// jobPoller is the narrow slice of apiclient.Client that WaitForJob depends
// on. The concrete *apiclient.Client satisfies it implicitly; tests inject a
// fake without spinning up an HTTP server.
type jobPoller interface {
	GetBackupJob(storeName, jobID string) (*apiclient.BackupJob, error)
	CancelBackupJob(storeName, jobID string) (*apiclient.BackupJob, error)
}

// WaitOptions configures WaitForJob.
type WaitOptions struct {
	StoreName string
	JobID     string
	Format    output.Format
	// Timeout is the maximum wall-clock time to poll before returning
	// ErrPollTimeout. Zero = indefinite (D-04).
	Timeout time.Duration
}

// Poll cadence — fixed 1s per D-05. Overridable from tests via pollInterval.
var pollInterval = 1 * time.Second

// interruptHandler is invoked on Ctrl-C during WaitForJob. It must return
// one of "d" (detach — default), "c" (cancel), or "C" (continue watching).
// Tests swap it to avoid reading from stdin; production reads a line from
// stderr/stdin via handleCtrlCStdin.
var interruptHandler = handleCtrlCStdin

// notifyInterrupt wires os.Interrupt into ctx for the poll loop. Tests swap
// it to a channel they control so they can simulate Ctrl-C without actually
// sending SIGINT to the test process.
var notifyInterrupt = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt)
}

// WaitForJob polls the controlplane at 1s intervals until the job reaches
// a terminal state or the caller detaches/times out. Matches D-01..D-11.
//
// Return shape:
//   - (job, nil)                — terminal state reached; caller maps
//     job.Status to exit code via SuccessExitCode (D-06).
//   - (nil, ErrPollTimeout)     — --timeout elapsed; caller exits 2 (D-11).
//     Stderr carries a poll hint.
//   - (nil, ErrPollDetached)    — user chose [d]etach; caller exits 0
//     (D-08). Stderr carries a poll hint.
//   - (nil, other)              — transport / API error; caller exits 1.
func WaitForJob(ctx context.Context, client jobPoller, opts WaitOptions) (*apiclient.BackupJob, error) {
	pollCtx, stop := notifyInterrupt(ctx)
	defer stop()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var deadline <-chan time.Time
	if opts.Timeout > 0 {
		deadline = time.After(opts.Timeout)
	}

	spinner := newSpinner(opts.Format)
	defer spinner.Stop()

	var lastStatus string

	for {
		select {
		case <-pollCtx.Done():
			// A cancelled parent ctx (not Ctrl-C) is treated as a
			// transport error so the CLI doesn't swallow a kill
			// signal from supervisors.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// Otherwise this was an os.Interrupt — prompt the user.
			action := interruptHandler(opts.StoreName, opts.JobID)
			switch action {
			case "d":
				_, _ = fmt.Fprintf(stderrOut,
					"Detached — job still running. Poll: dfsctl store metadata %s backup job show %s\n",
					opts.StoreName, opts.JobID)
				return nil, ErrPollDetached
			case "c":
				if _, err := client.CancelBackupJob(opts.StoreName, opts.JobID); err != nil {
					_, _ = fmt.Fprintf(stderrOut, "Cancel failed: %v (continuing to poll)\n", err)
				}
				// Re-arm the signal watcher for a future Ctrl-C.
				stop()
				pollCtx, stop = notifyInterrupt(ctx)
			case "C":
				stop()
				pollCtx, stop = notifyInterrupt(ctx)
			}
		case <-deadline:
			_, _ = fmt.Fprintf(stderrOut,
				"Timeout — job still running. Poll: dfsctl store metadata %s backup job show %s\n",
				opts.StoreName, opts.JobID)
			return nil, ErrPollTimeout
		case <-ticker.C:
			job, err := client.GetBackupJob(opts.StoreName, opts.JobID)
			if err != nil {
				return nil, err
			}
			if job.Status != lastStatus {
				spinner.Transition(lastStatus, job.Status)
				lastStatus = job.Status
			}
			spinner.Tick()
			if terminalStatuses[job.Status] {
				return job, nil
			}
		}
	}
}

// SuccessExitCode maps a terminal job status to a process exit code per D-06.
// Non-terminal inputs default to 1 (caller error — should not happen).
func SuccessExitCode(status string) int {
	if status == "succeeded" {
		return 0
	}
	return 1
}

// handleCtrlCStdin prompts on stderr and reads a single-line response from
// stdin. Empty / unrecognized response defaults to "d" (detach) per D-03.
func handleCtrlCStdin(storeName, jobID string) string {
	_ = storeName // included in caller-side hints only
	_ = jobID
	fmt.Fprint(os.Stderr,
		"\nInterrupted. [d]etach (default) / [c]ancel / [C]ontinue watching: ")
	var response string
	_, _ = fmt.Scanln(&response)
	switch strings.TrimSpace(response) {
	case "c":
		return "c"
	case "C":
		return "C"
	default:
		return "d"
	}
}

// ----------------------------------------------------------------------------
// Spinner
// ----------------------------------------------------------------------------

// spinner is a minimal stderr-only animation. Transitions are logged one per
// line ("pending -> running"); frames are emitted with `\r` so they overwrite
// in a tty. In JSON / YAML output modes the spinner is a complete no-op to
// keep stdout clean for piping (D-07).
type spinner struct {
	out     io.Writer
	enabled bool
	frames  []string
	frame   int
}

// spinnerOut is the writer the spinner targets; overridable from tests.
var spinnerOut io.Writer = os.Stderr

// stderrOut is the writer WaitForJob uses for human hints (detach/timeout).
// Overridable from tests so we don't need to muck with os.Stderr.
var stderrOut io.Writer = os.Stderr

func newSpinner(format output.Format) *spinner {
	return &spinner{
		out:     spinnerOut,
		enabled: format == output.FormatTable || format == "",
		frames:  []string{"|", "/", "-", "\\"},
	}
}

// Tick advances the spinner by one frame. No-op when disabled.
func (s *spinner) Tick() {
	if !s.enabled {
		return
	}
	frame := s.frames[s.frame%len(s.frames)]
	s.frame++
	_, _ = fmt.Fprintf(s.out, "\r%s watching job...", frame)
}

// Transition logs a status change on its own line (D-02). from may be empty
// on first transition. No-op when disabled.
func (s *spinner) Transition(from, to string) {
	if !s.enabled {
		return
	}
	// Overwrite any in-flight spinner frame before the transition line.
	_, _ = fmt.Fprint(s.out, "\r")
	if from == "" {
		_, _ = fmt.Fprintf(s.out, "Status: %s\n", to)
		return
	}
	_, _ = fmt.Fprintf(s.out, "Status: %s -> %s\n", from, to)
}

// Stop clears the spinner line. No-op when disabled.
func (s *spinner) Stop() {
	if !s.enabled {
		return
	}
	// Clear the spinner's current line so the caller's summary renders
	// on a clean line.
	_, _ = fmt.Fprint(s.out, "\r \r")
}
