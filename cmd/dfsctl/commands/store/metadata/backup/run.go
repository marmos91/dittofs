package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// Flags — package-level so tests can set them directly without going through
// pflag parsing.
var (
	runRepo    string
	runWait    bool
	runAsync   bool
	runTimeout time.Duration
)

// exitFunc is os.Exit in production. Tests swap it to capture the intended
// exit code without terminating the test binary.
var exitFunc = os.Exit

// stdoutOut is the writer run.go uses for structured output (JSON/YAML) and
// human success summaries. Overridable from tests; production = os.Stdout.
var stdoutOut io.Writer = os.Stdout

// runCmd is the explicit `backup run <store-name>` verb that triggers an
// on-demand backup. Registered as a child of backup.Cmd.
var runCmd = &cobra.Command{
	Use:   "run <store-name>",
	Short: "Trigger an on-demand backup of a metadata store",
	Long: `Trigger an on-demand backup of a metadata store.

By default the command blocks until the backup reaches a terminal state
(D-01). Use --async to return immediately with the job record.

Examples:
  # Block until backup completes (default)
  dfsctl store metadata fast-meta backup run --repo daily-s3

  # Return immediately with the job record
  dfsctl store metadata fast-meta backup run --repo daily-s3 --async

  # Set a wait timeout
  dfsctl store metadata fast-meta backup run --repo daily-s3 --timeout 30s`,
	Args: cobra.ExactArgs(1),
	RunE: runRun,
}

func init() {
	runCmd.Flags().StringVar(&runRepo, "repo", "",
		"Backup repo name (required when >1 repo attached per D-24)")
	runCmd.Flags().BoolVar(&runWait, "wait", true,
		"Block until the backup job terminates (D-01 default)")
	runCmd.Flags().BoolVar(&runAsync, "async", false,
		"Return immediately with the job record (D-01 opt-out of default wait)")
	runCmd.Flags().DurationVar(&runTimeout, "timeout", 0,
		"Wait timeout (0 = indefinite per D-04)")
}

// runRun is the Cobra RunE for the parent `backup` verb. Consumes Plan 02's
// TriggerBackupResponse{Record, Job} directly — no ListBackupJobs fallback
// (Blockers 2+3 closed by Plan 02's envelope).
func runRun(cmd *cobra.Command, args []string) error {
	storeName := args[0]

	// --async beats --wait if the user passed both (scripts typically set
	// --async explicitly; --wait is only the default).
	wait := runWait && !runAsync

	client, err := getClient()
	if err != nil {
		return err
	}

	resp, err := client.TriggerBackup(storeName, &apiclient.TriggerBackupRequest{Repo: runRepo})
	if err != nil {
		// D-13 BackupAlreadyRunningError: surface the running job id + hint.
		var already *apiclient.BackupAlreadyRunningError
		if errors.As(err, &already) {
			_, _ = fmt.Fprintf(stderrOut, "Backup already running: %s.\n", already.RunningJobID)
			_, _ = fmt.Fprintf(stderrOut, "Show: dfsctl store metadata %s backup job show %s\n",
				storeName, already.RunningJobID)
			return fmt.Errorf("backup already running")
		}
		return fmt.Errorf("failed to trigger backup: %w", err)
	}

	if resp == nil || resp.Record == nil || resp.Job == nil {
		return fmt.Errorf("server returned incomplete trigger response (Plan 02 envelope missing)")
	}

	// --async (D-09): emit the job record on stdout, poll hint on stderr.
	if !wait {
		return emitAsyncOutput(storeName, resp.Record, resp.Job)
	}

	// --wait default (D-01 / D-02).
	_, _ = fmt.Fprintf(stderrOut, "Backup job %s started (record %s). Polling...\n",
		resp.Job.ID, resp.Record.ID)

	format := parseFormat()
	// cmd.Context() is nil when Cobra is invoked directly (e.g., unit tests
	// calling runRun without cobra.Execute). Fall back to a background ctx
	// so the signal.NotifyContext inside WaitForJob has a valid parent.
	pollCtx := cmd.Context()
	if pollCtx == nil {
		pollCtx = context.Background()
	}
	job, waitErr := WaitForJob(pollCtx, client, WaitOptions{
		StoreName: storeName,
		JobID:     resp.Job.ID,
		Timeout:   runTimeout,
		Format:    format,
	})
	// D-08 detach: exit 0. Caller pipeline receives clean stdout + stderr hint.
	if errors.Is(waitErr, ErrPollDetached) {
		return nil
	}
	// D-11 timeout: exit 2 (distinct from terminal failure).
	if errors.Is(waitErr, ErrPollTimeout) {
		exitFunc(2)
		return nil
	}
	if waitErr != nil {
		return waitErr
	}

	// D-10 success summary. JSON mode emits the bare BackupJob; table mode
	// prints a human banner + the BackupRecord.ID so operators can chain
	// `backup show <id>` / `backup pin <id>` immediately.
	switch format {
	case output.FormatJSON:
		if err := output.PrintJSON(stdoutOut, job); err != nil {
			return err
		}
	case output.FormatYAML:
		if err := output.PrintYAML(stdoutOut, job); err != nil {
			return err
		}
	default:
		_, _ = fmt.Fprintf(stdoutOut, "\u2713 Backup completed\n")
		_, _ = fmt.Fprintf(stdoutOut, "  Record:    %s\n", resp.Record.ID)
		_, _ = fmt.Fprintf(stdoutOut, "  Duration:  %s\n", renderDuration(job))
		_, _ = fmt.Fprintf(stdoutOut, "  Size:      %s\n", humanSize(resp.Record.SizeBytes))
	}

	exitFunc(SuccessExitCode(job.Status))
	return nil
}

// emitAsyncOutput implements D-09 — stdout gets the job record (JSON/YAML
// passes through; table mode renders the same grouped-section detail as
// `backup job show`), stderr gets the poll hint in non-JSON modes.
func emitAsyncOutput(storeName string, rec *apiclient.BackupRecord, job *apiclient.BackupJob) error {
	format := parseFormat()
	switch format {
	case output.FormatJSON:
		if err := output.PrintJSON(stdoutOut, job); err != nil {
			return err
		}
	case output.FormatYAML:
		if err := output.PrintYAML(stdoutOut, job); err != nil {
			return err
		}
	default:
		if err := output.PrintTable(stdoutOut, asyncJobSummary{record: rec, job: job}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stderrOut, "Poll: dfsctl store metadata %s backup job show %s\n",
			storeName, job.ID)
	}
	return nil
}

// asyncJobSummary is a compact TableRenderer used ONLY by --async table mode.
// The full grouped-section renderer lives in the job/ sub-package; this keeps
// run.go free of a cross-package import cycle while still giving operators
// the essential (Job ID + Record ID + Status) needed to chain the next verb.
type asyncJobSummary struct {
	record *apiclient.BackupRecord
	job    *apiclient.BackupJob
}

func (a asyncJobSummary) Headers() []string { return []string{"FIELD", "VALUE"} }
func (a asyncJobSummary) Rows() [][]string {
	return [][]string{
		{"Job ID", a.job.ID},
		{"Record ID", a.record.ID},
		{"Kind", a.job.Kind},
		{"Repo", a.job.RepoID},
		{"Status", a.job.Status},
	}
}

// parseFormat is a thin wrapper that falls back to FormatTable on parse
// errors (the top-level cobra pre-run rejects invalid -o values anyway).
func parseFormat() output.Format {
	f, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return output.FormatTable
	}
	return f
}

// renderDuration renders the job's started->finished span, or "-" if either
// endpoint is missing (e.g., succeeded but no FinishedAt yet in a race).
func renderDuration(job *apiclient.BackupJob) string {
	if job == nil || job.StartedAt == nil || job.FinishedAt == nil {
		return "-"
	}
	return job.FinishedAt.Sub(*job.StartedAt).Round(time.Second).String()
}
