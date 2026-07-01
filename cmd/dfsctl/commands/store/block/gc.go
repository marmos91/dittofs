package block

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// Terminal GC-job states (mirror runtime.GCState*).
const (
	gcStateDone   = "done"
	gcStateFailed = "failed"
)

// gcCmd triggers an on-demand block-store GC run for the named share and
// prints the engine.GCStats summary.
var gcCmd = &cobra.Command{
	Use:   "gc <share>",
	Short: "Run garbage collection for a block store share",
	Long: `Trigger an on-demand GC run for the named share.

The mark phase enumerates every live ContentHash across all shares whose
remote-store config matches the named share (cross-share aggregation).
The sweep phase reclaims storage absent from the live set: it decrements
the refcount of each dead chunk's enclosing packed block and, once a
block holds no live chunks, deletes it from the remote and evicts its
local copy. Legacy per-chunk cas/.../ objects (written before packed
blocks) are also deleted when dead and older than the configured grace
period (default 1h). The last-run.json summary is persisted under the
share's gc-state directory and can be inspected with:

  dfsctl store block gc-status <share>

The run executes asynchronously on the server (the mark phase can take
minutes on a large or snapshot-heavy deployment). By default this command
polls until the job finishes, rendering progress; pass --no-wait to print
the job id and return immediately.

Use --dry-run to skip deletes and print up to dry_run_sample_size
candidate keys (default 1000). Recommended for first-time deployment
confidence and for debugging suspected mark-phase bugs.

Use --reconcile to additionally reap stranded file_blocks rows — rows
whose owning file was deleted before the unlink-refcount fix, which a
plain GC cannot reclaim because they keep their hashes in the live set.
Reconcile is server-wide (all shares) and the recommended way to recover
space leaked by older versions. Combine with --dry-run to preview.

Use --grace-period to override the configured sweep grace for this run
only. A zero grace (--grace-period 0) reaps every eligible orphan with no
age guard, bypassing the server's 5-minute floor — useful to reclaim
just-orphaned chunks immediately in tests or one-off cleanups. Cannot be
combined with --reconcile.

Examples:
  dfsctl store block gc myshare
  dfsctl store block gc myshare --dry-run
  dfsctl store block gc myshare --reconcile
  dfsctl store block gc myshare --grace-period 0
  dfsctl store block gc myshare --grace-period 30m
  dfsctl store block gc myshare --no-wait
  dfsctl store block gc myshare -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runBlockStoreGC,
}

func init() {
	gcCmd.Flags().Bool("dry-run", false, "Run mark + sweep enumeration but skip deletes; print candidate keys")
	gcCmd.Flags().Bool("reconcile", false, "Also reap stranded file_blocks rows leaked by older versions (server-wide), then sweep both tiers")
	gcCmd.Flags().Bool("no-wait", false, "Start the job and print its id without waiting for completion")
	gcCmd.Flags().Duration("grace-period", 0, "Override the sweep grace for this run (e.g. 30m, 0 to reap immediately); bypasses the server's 5m floor. Unset = server default")
}

func runBlockStoreGC(cmd *cobra.Command, args []string) error {
	share := args[0]
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	reconcile, _ := cmd.Flags().GetBool("reconcile")
	noWait, _ := cmd.Flags().GetBool("no-wait")

	opts := &apiclient.BlockStoreGCOptions{DryRun: dryRun, Reconcile: reconcile}
	if cmd.Flags().Changed("grace-period") {
		grace, _ := cmd.Flags().GetDuration("grace-period")
		if grace < 0 {
			return fmt.Errorf("--grace-period must not be negative")
		}
		if reconcile {
			return fmt.Errorf("--grace-period cannot be combined with --reconcile")
		}
		secs := int64(grace / time.Second)
		opts.GracePeriodSeconds = &secs
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	jobID, err := client.StartBlockStoreGC(share, opts)
	if err != nil {
		return fmt.Errorf("failed to start block store GC: %w", err)
	}

	format, ferr := cmdutil.GetOutputFormatParsed()
	if ferr != nil {
		return ferr
	}

	if noWait {
		switch format {
		case output.FormatJSON:
			return output.PrintJSON(os.Stdout, map[string]string{"job_id": jobID})
		case output.FormatYAML:
			return output.PrintYAML(os.Stdout, map[string]string{"job_id": jobID})
		default:
			fmt.Printf("GC job started: %s\n", jobID)
		}
		return nil
	}

	return watchGC(client, share, jobID, format)
}

// watchGC polls the GC job until it reaches a terminal state. In table mode it
// renders a live progress status line; in JSON/YAML mode it stays silent until
// the terminal status so the machine-readable output is not polluted. On
// completion it emits the full stats body and returns a non-zero error if the
// run hit sweep errors, preserving the prior synchronous command's exit
// semantics.
func watchGC(client *apiclient.Client, share, jobID string, format output.Format) error {
	renderProgress := format == output.FormatTable
	for {
		status, err := client.GetBlockStoreGCJob(share, jobID)
		if err != nil {
			return fmt.Errorf("failed to poll GC job: %w", err)
		}

		switch status.State {
		case gcStateDone:
			if renderProgress {
				fmt.Printf("\rmarked %d hashes, swept %d objects (%s) — done                \n",
					status.HashesMarked, status.ObjectsSwept, formatBytes(status.BytesFreed))
			}
			return emitGCResult(status, format)
		case gcStateFailed:
			if renderProgress {
				fmt.Println()
			}
			return fmt.Errorf("GC job failed: %s", status.Error)
		default:
			if renderProgress {
				fmt.Printf("\rmarked %d hashes, scanned %d, swept %d objects (%s)        ",
					status.HashesMarked, status.ObjectsScanned, status.ObjectsSwept,
					formatBytes(status.BytesFreed))
			}
		}

		time.Sleep(1 * time.Second)
	}
}

// emitGCResult renders the terminal job's stats in the requested output format
// and returns an error when the run reported sweep errors (so scripts gating on
// the exit code see it in every format, not only the table).
func emitGCResult(status *apiclient.GCJobStatus, format output.Format) error {
	switch format {
	case output.FormatJSON:
		if err := output.PrintJSON(os.Stdout, status); err != nil {
			return err
		}
	case output.FormatYAML:
		if err := output.PrintYAML(os.Stdout, status); err != nil {
			return err
		}
	default:
		if err := printGCStatsTable(status); err != nil {
			return err
		}
	}

	if status.Stats != nil && status.Stats.ErrorCount > 0 {
		return fmt.Errorf("GC completed with %d sweep error(s)", status.Stats.ErrorCount)
	}
	return nil
}

// printGCStatsTable renders the GC summary as a key/value table plus an
// optional dry-run candidate listing. Mirrors stats.go's output style.
func printGCStatsTable(status *apiclient.GCJobStatus) error {
	s := status.Stats
	if s == nil {
		// Terminal job without a persisted stats body (should not happen for a
		// done run): fall back to the live counters.
		pairs := [][2]string{
			{"Hashes Marked", fmt.Sprintf("%d", status.HashesMarked)},
			{"Objects Found", fmt.Sprintf("%d", status.ObjectsScanned)},
			{"Objects Swept", fmt.Sprintf("%d", status.ObjectsSwept)},
			{"Bytes Freed", formatBytes(status.BytesFreed)},
		}
		return output.SimpleTable(os.Stdout, pairs)
	}

	pairs := [][2]string{
		{"Run ID", s.RunID},
		{"Hashes Marked", fmt.Sprintf("%d", s.HashesMarked)},
		{"Objects Found", fmt.Sprintf("%d", s.ObjectsScanned)},
		{"Objects Swept", fmt.Sprintf("%d", s.ObjectsSwept)},
		{"Bytes Freed", formatBytes(s.BytesFreed)},
		{"Duration", fmt.Sprintf("%dms", s.DurationMs)},
		{"Errors", fmt.Sprintf("%d", s.ErrorCount)},
		{"Dry Run", fmt.Sprintf("%v", s.DryRun)},
	}
	if err := output.SimpleTable(os.Stdout, pairs); err != nil {
		return err
	}

	if len(s.FirstErrors) > 0 {
		fmt.Println()
		fmt.Println("First errors:")
		for _, e := range s.FirstErrors {
			fmt.Printf("  - %s\n", e)
		}
	}

	// Use the JOB's actual dry-run flag, not the CLI request: StartBlockStoreGC
	// may return an already-running job started with different flags, so the
	// rendering must reflect what the job did, not what this invocation asked.
	if status.DryRun || len(s.DryRunCandidates) > 0 {
		fmt.Println()
		fmt.Printf("Dry-run candidates (%d):\n", len(s.DryRunCandidates))
		for _, c := range s.DryRunCandidates {
			fmt.Printf("  %s\n", c)
		}
	}

	return nil
}
