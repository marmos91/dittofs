package block

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// ErrNoGCRunYet signals that the share has no `last-run.json` to read —
// either GC has never run for this share, or the local store backend is
// non-persistent (in-memory). The caller can detect this with errors.Is
// to distinguish from genuine HTTP / network failures.
var ErrNoGCRunYet = errors.New("no GC run recorded for share yet")

// gcStatusCmd reads <gcStateRoot>/last-run.json for the named share and
// prints the parsed engine.GCRunSummary so operators can confirm the
// last GC run completed cleanly without tailing logs.
var gcStatusCmd = &cobra.Command{
	Use:   "gc-status <share>",
	Short: "Show the last block-store GC run summary for a share",
	Long: `Print the most recent garbage collection run summary for the named share.

Reads <gcStateRoot>/last-run.json, which is overwritten by every
completed GC run. Returns exit 1 with a friendly message if no run has
been recorded yet (the share has never been GC'd, or its local store
has no persistent root).

Examples:
  dfsctl store block gc-status myshare
  dfsctl store block gc-status myshare -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runBlockStoreGCStatus,
}

func runBlockStoreGCStatus(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	summary, err := client.BlockStoreGCStatus(share)
	if err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			// "No run yet" is a distinct, expected state. Surface it as a
			// recognizable error so scripts and tests can branch on it
			// (cobra's RunE-error path already produces a non-zero exit).
			return ErrNoGCRunYet
		}
		return fmt.Errorf("failed to read GC status: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, summary)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, summary)
	default:
		pairs := [][2]string{
			{"Run ID", summary.RunID},
			{"Started At", summary.StartedAt.Format("2006-01-02T15:04:05Z07:00")},
			{"Completed At", summary.CompletedAt.Format("2006-01-02T15:04:05Z07:00")},
			{"Duration", fmt.Sprintf("%dms", summary.DurationMs)},
			{"Hashes Marked", fmt.Sprintf("%d", summary.HashesMarked)},
			{"Objects Swept", fmt.Sprintf("%d", summary.ObjectsSwept)},
			{"Bytes Freed", formatBytes(summary.BytesFreed)},
			{"Errors", fmt.Sprintf("%d", summary.ErrorCount)},
			{"Dry Run", fmt.Sprintf("%v", summary.DryRun)},
		}
		if err := output.SimpleTable(os.Stdout, pairs); err != nil {
			return err
		}
		if len(summary.FirstErrors) > 0 {
			fmt.Println()
			fmt.Println("First errors:")
			for _, e := range summary.FirstErrors {
				fmt.Printf("  - %s\n", e)
			}
		}
		if len(summary.DryRunCandidates) > 0 {
			fmt.Println()
			fmt.Printf("Dry-run candidates (%d):\n", len(summary.DryRunCandidates))
			for _, c := range summary.DryRunCandidates {
				fmt.Printf("  %s\n", c)
			}
		}
		return nil
	}
}
