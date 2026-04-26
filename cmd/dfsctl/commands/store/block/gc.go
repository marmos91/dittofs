package block

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// gcCmd triggers an on-demand block-store GC run for the named share and
// prints the engine.GCStats summary.
var gcCmd = &cobra.Command{
	Use:   "gc <share>",
	Short: "Run garbage collection for a block store share",
	Long: `Trigger an on-demand GC run for the named share.

The mark phase enumerates every live ContentHash across all shares whose
remote-store config matches the named share (cross-share aggregation).
The sweep phase deletes any cas/.../ object absent from the live set
whose LastModified is older than the configured grace period (default
1h). The last-run.json summary is persisted under the share's gc-state
directory and can be inspected with:

  dfsctl store block gc-status <share>

Use --dry-run to skip deletes and print up to dry_run_sample_size
candidate keys (default 1000). Recommended for first-time deployment
confidence and for debugging suspected mark-phase bugs.

Examples:
  dfsctl store block gc myshare
  dfsctl store block gc myshare --dry-run
  dfsctl store block gc myshare -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runBlockStoreGC,
}

func init() {
	gcCmd.Flags().Bool("dry-run", false, "Run mark + sweep enumeration but skip deletes; print candidate keys")
}

func runBlockStoreGC(cmd *cobra.Command, args []string) error {
	share := args[0]
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	res, err := client.BlockStoreGC(share, &apiclient.BlockStoreGCOptions{DryRun: dryRun})
	if err != nil {
		return fmt.Errorf("failed to run block store GC: %w", err)
	}
	if res == nil || res.Stats == nil {
		return fmt.Errorf("block store GC: server returned empty response")
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, res)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, res)
	default:
		return printGCStatsTable(res, dryRun)
	}
}

// printGCStatsTable renders the GC summary as a key/value table plus an
// optional dry-run candidate listing. Mirrors stats.go's output style.
func printGCStatsTable(res *apiclient.BlockStoreGCResult, dryRun bool) error {
	s := res.Stats
	pairs := [][2]string{
		{"Run ID", s.RunID},
		{"Hashes Marked", fmt.Sprintf("%d", s.HashesMarked)},
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

	if dryRun || len(s.DryRunCandidates) > 0 {
		fmt.Println()
		fmt.Printf("Dry-run candidates (%d):\n", len(s.DryRunCandidates))
		for _, c := range s.DryRunCandidates {
			fmt.Printf("  %s\n", c)
		}
	}

	return nil
}
