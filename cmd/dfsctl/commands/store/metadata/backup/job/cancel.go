package job

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var cancelCmd = &cobra.Command{
	Use:   "cancel <job-id>",
	Short: "Cancel a running backup/restore job",
	Long: `Request cancellation of a running backup or restore job (D-43).

Cancellation is fire-and-forget: the command returns after one POST and does
not implicitly wait for the job to reach a terminal state (D-44). A cancel
against an already-terminal job is idempotent and returns the unchanged job
(D-45) — the next-step hint is printed either way.

Examples:
  dfsctl store metadata fast-meta backup job cancel 01HABCDEFGHJKMNPQRST`,
	Args: cobra.ExactArgs(2), // <store-name> <job-id>
	RunE: runCancel,
}

func runCancel(cmd *cobra.Command, args []string) error {
	storeName, jobID := args[0], args[1]

	client, err := clientFactory()
	if err != nil {
		return err
	}

	job, err := client.CancelBackupJob(storeName, jobID)
	if err != nil {
		return fmt.Errorf("failed to cancel job: %w", err)
	}

	// D-45: cancel on terminal = 200 OK idempotent. Same hint either way.
	// In JSON / YAML mode the banner is omitted and the updated job is
	// serialised to stdout so scripts can consume it.
	format, fmtErr := cmdutil.GetOutputFormatParsed()
	if fmtErr != nil {
		format = output.FormatTable
	}
	if format == output.FormatJSON || format == output.FormatYAML {
		return cmdutil.PrintResource(stdoutOut, job, nil)
	}

	// Table mode: write banner + hint to the injected sink via output.Printer
	// so colors render in production and tests still capture the bytes.
	banner := fmt.Sprintf("Cancel requested for job %s.", jobID)
	hint := fmt.Sprintf("Poll: dfsctl store metadata %s backup job show %s", storeName, jobID)
	output.NewPrinter(stdoutOut, output.FormatTable, !cmdutil.IsColorDisabled()).Success(banner)
	if _, err := fmt.Fprintln(stdoutOut, hint); err != nil {
		return err
	}
	return nil
}
