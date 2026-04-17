// Package restore implements the metadata-store restore verb.
package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// -----------------------------------------------------------------------------
// Flags
// -----------------------------------------------------------------------------

var (
	restoreFromID  string
	restoreYes     bool
	restoreForce   bool // -f alias for --yes
	restoreDryRun  bool
	restoreWait    bool
	restoreAsync   bool
	restoreTimeout time.Duration
)

// -----------------------------------------------------------------------------
// Test seams — package-private vars swapped by unit tests via t.Cleanup.
// Production defaults are wired to the real CLI primitives.
// -----------------------------------------------------------------------------

// clientFactory returns the authenticated apiclient.Client. Swapped in tests
// to inject an httptest-backed fake without touching the real credential
// store (mirrors backup/backup.go pattern).
var clientFactory = cmdutil.GetAuthenticatedClient

func getClient() (*apiclient.Client, error) { return clientFactory() }

// confirmFunc is the D-30 confirmation prompt. Tests swap it to return
// deterministic answers without driving promptui.
var confirmFunc = prompt.ConfirmWithForce

// waitFn wraps backup.WaitForJob so tests can simulate detach / timeout /
// transport errors without relying on backup-package-private globals
// (pollInterval, notifyInterrupt, interruptHandler) we cannot access from
// another package.
var waitFn = func(ctx context.Context, client *apiclient.Client, opts backup.WaitOptions) (*apiclient.BackupJob, error) {
	return backup.WaitForJob(ctx, client, opts)
}

// Output sinks — overridable from tests.
var (
	stdoutOut io.Writer = os.Stdout
	stderrOut io.Writer = os.Stderr
)

// exitFunc is os.Exit in production. Tests swap it to capture the intended
// exit code without terminating the test binary.
var exitFunc = os.Exit

// -----------------------------------------------------------------------------
// Command
// -----------------------------------------------------------------------------

var Cmd = &cobra.Command{
	Use:   "restore <store-name>",
	Short: "Restore a metadata store from a backup record",
	Long: `Restore a metadata store from a backup record.

By default restores the latest succeeded backup; use --from <ULID> to pick a
specific one.

REQUIRES all shares referencing the store to be disabled first:
  dfsctl share <name> disable

The restore:
  1. Disables traffic at the protocol layer (NFS MOUNT / SMB TREE_CONNECT refuse)
  2. Drains live handles
  3. Downloads + SHA-verifies the selected archive
  4. Atomically swaps the metadata store under a temporary path
  5. Reopens and resumes

On failure, the original store is untouched.

Examples:
  # Restore latest (prompts for confirmation)
  dfsctl store metadata fast-meta restore

  # Restore specific ULID, skip prompt
  dfsctl store metadata fast-meta restore --from 01HABCDEFGHJKMNPQRSTUVWXY1 --yes

  # Server-side dry run (pre-flight only; skips shares-enabled gate per D-31)
  dfsctl store metadata fast-meta restore --dry-run`,
	Args: cobra.ExactArgs(1),
	RunE: runRestore,
}

func init() {
	Cmd.Flags().StringVar(&restoreFromID, "from", "", "Record ULID to restore from (26 chars; default = latest succeeded)")
	Cmd.Flags().BoolVar(&restoreYes, "yes", false, "Skip confirmation prompt")
	Cmd.Flags().BoolVarP(&restoreForce, "force", "f", false, "Alias for --yes")
	Cmd.Flags().BoolVar(&restoreDryRun, "dry-run", false, "Server-side pre-flight only (record resolution + manifest fetch + validation); skips shares-enabled gate (D-31)")
	Cmd.Flags().BoolVar(&restoreWait, "wait", true, "Block until the restore job terminates (default)")
	Cmd.Flags().BoolVar(&restoreAsync, "async", false, "Return immediately with the job record")
	Cmd.Flags().DurationVar(&restoreTimeout, "timeout", 0, "Wait timeout (0 = indefinite)")
}

// runRestore is the Cobra RunE for the restore verb.
func runRestore(cmd *cobra.Command, args []string) error {
	storeName := args[0]

	// D-40: --from requires exactly 26-char ULID (shared check for both
	// dry-run + real restore — applied BEFORE any HTTP call).
	if restoreFromID != "" && len(restoreFromID) != 26 {
		return fmt.Errorf("invalid --from: must be a 26-character ULID (got %d chars)", len(restoreFromID))
	}

	client, err := getClient()
	if err != nil {
		return err
	}

	// D-31 — server-side dry-run skips shares-enabled gate + confirmation.
	if restoreDryRun {
		return runDryRun(client, storeName, restoreFromID)
	}

	// D-30 — confirmation prompt unless --yes / -f.
	skipPrompt := restoreYes || restoreForce
	if !skipPrompt {
		label := fmt.Sprintf(`Restore %s
  From backup: %s

This will REPLACE all metadata in %s. Continue?`, storeName, pickFromLabel(restoreFromID), storeName)
		confirmed, err := confirmFunc(label, false)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
		if !confirmed {
			fmt.Fprintln(stdoutOut, "Aborted.")
			return nil
		}
	}

	// --async beats --wait if both are passed (scripts typically set --async
	// explicitly; --wait is only the default).
	wait := restoreWait && !restoreAsync

	// Trigger restore — Plan 02 returns the BackupJob directly (Plan 01
	// Task 2 amended RunRestore signature).
	req := &apiclient.RestoreRequest{FromBackupID: restoreFromID}
	job, err := client.StartRestore(storeName, req)
	if err != nil {
		// D-29 — hard 409 on shares-enabled precondition.
		var precond *apiclient.RestorePreconditionError
		if errors.As(err, &precond) {
			return renderPreconditionFailure(storeName, precond.EnabledShares)
		}
		return fmt.Errorf("failed to start restore: %w", err)
	}

	if !wait {
		return emitAsyncRestoreOutput(storeName, job)
	}

	fmt.Fprintf(stderrOut, "Restore job %s started. Polling...\n", job.ID)
	pollCtx := cmd.Context()
	if pollCtx == nil {
		pollCtx = context.Background()
	}
	finalJob, waitErr := waitFn(pollCtx, client, backup.WaitOptions{
		StoreName: storeName,
		JobID:     job.ID,
		Timeout:   restoreTimeout,
		Format:    parseFormat(),
	})
	// D-08 detach: stderr-only message + stdout clean + exit 0 (aligned with
	// Plan 05 run.go). The hint is already printed by WaitForJob.
	if errors.Is(waitErr, backup.ErrPollDetached) {
		return nil
	}
	// D-11 timeout: exit 2 (distinct from terminal failure).
	if errors.Is(waitErr, backup.ErrPollTimeout) {
		exitFunc(2)
		return nil
	}
	if waitErr != nil {
		return waitErr
	}

	// D-33 — success output with re-enable hint.
	format := parseFormat()
	switch format {
	case output.FormatJSON:
		if err := output.PrintJSON(stdoutOut, finalJob); err != nil {
			return err
		}
	case output.FormatYAML:
		if err := output.PrintYAML(stdoutOut, finalJob); err != nil {
			return err
		}
	default:
		fmt.Fprintf(stdoutOut, "\u2713 Restore %s\n", finalJob.Status)
		if finalJob.Status == "succeeded" {
			fmt.Fprintln(stdoutOut, "Shares remain disabled. Re-enable with:")
			fmt.Fprintln(stdoutOut, "  dfsctl share <name> enable")
		}
	}
	exitFunc(backup.SuccessExitCode(finalJob.Status))
	return nil
}

// renderPreconditionFailure prints the D-29 output on stderr and returns a
// non-nil error so cobra exits non-zero:
//
//	Cannot restore: N share(s) enabled — disable them first.
//	  dfsctl share <a> disable
//	  dfsctl share <b> disable
func renderPreconditionFailure(_ string, enabledShares []string) error {
	fmt.Fprintf(stderrOut, "Cannot restore: %d share(s) enabled \u2014 disable them first.\n", len(enabledShares))
	for _, s := range enabledShares {
		fmt.Fprintf(stderrOut, "  dfsctl share '%s' disable\n", s)
	}
	return fmt.Errorf("restore precondition failed: %d share(s) still enabled", len(enabledShares))
}

func pickFromLabel(fromID string) string {
	if fromID == "" {
		return "<latest succeeded>"
	}
	return fromID
}

// runDryRun performs the D-31 server-side pre-flight via Plan 02's
// POST /api/v1/store/metadata/{name}/restore/dry-run endpoint. Returns
// DryRunResult{Record, ManifestValid, EnabledShares}. Shares-enabled
// gate is SKIPPED server-side (D-31 rehearsal semantics) but the list
// of currently-enabled shares is reported for operator awareness.
func runDryRun(client *apiclient.Client, storeName, fromID string) error {
	req := &apiclient.RestoreRequest{FromBackupID: fromID}
	result, err := client.RestoreDryRun(storeName, req)
	if err != nil {
		// Typed error surfacing (e.g. ErrNoRestoreCandidate → 409,
		// ErrManifestVersionUnsupported → 400).
		return fmt.Errorf("dry-run failed: %w", err)
	}

	switch parseFormat() {
	case output.FormatJSON:
		return output.PrintJSON(stdoutOut, result)
	case output.FormatYAML:
		return output.PrintYAML(stdoutOut, result)
	}

	// Table mode.
	fmt.Fprintln(stdoutOut, "Dry run: pre-flight only (no data mutation, no payload download).")
	fmt.Fprintf(stdoutOut, "  Target store:     %s\n", storeName)
	if result.Record != nil {
		fmt.Fprintf(stdoutOut, "  Selected record:  %s  (created %s, size %s)\n",
			result.Record.ID,
			result.Record.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
			humanSize(result.Record.SizeBytes))
	} else {
		fmt.Fprintln(stdoutOut, "  Selected record:  <none>")
	}
	if result.ManifestValid {
		fmt.Fprintln(stdoutOut, "  Manifest:         valid")
	} else {
		fmt.Fprintln(stdoutOut, "  Manifest:         INVALID (restore will refuse)")
	}
	if len(result.EnabledShares) > 0 {
		fmt.Fprintf(stdoutOut, "\n  Note: %d share(s) currently enabled on %s:\n", len(result.EnabledShares), storeName)
		for _, s := range result.EnabledShares {
			fmt.Fprintf(stdoutOut, "    %s\n", s)
		}
		fmt.Fprintln(stdoutOut, "  Disable them before running the real restore:")
		for _, s := range result.EnabledShares {
			fmt.Fprintf(stdoutOut, "    dfsctl share '%s' disable\n", s)
		}
	}
	return nil
}

// emitAsyncRestoreOutput implements the --async UX — stdout gets the job
// record (JSON/YAML passes through; table mode prints a compact summary),
// stderr gets the poll hint in non-JSON modes.
func emitAsyncRestoreOutput(storeName string, job *apiclient.BackupJob) error {
	switch parseFormat() {
	case output.FormatJSON:
		return output.PrintJSON(stdoutOut, job)
	case output.FormatYAML:
		return output.PrintYAML(stdoutOut, job)
	}
	fmt.Fprintf(stdoutOut, "Restore job %s started (status: %s).\n", job.ID, job.Status)
	fmt.Fprintf(stderrOut, "Poll: dfsctl store metadata %s backup job show %s\n", storeName, job.ID)
	return nil
}

// parseFormat mirrors backup/run.go's helper: fall back to table on a bad
// -o value (the top-level cobra pre-run rejects invalid values anyway).
func parseFormat() output.Format {
	f, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return output.FormatTable
	}
	return f
}

// humanSize renders a byte count using binary units ("1.0MB", "234KB",
// "12B"). Kept inline rather than exporting from the backup package —
// eight trivial lines aren't worth the indirection for a single caller.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}
