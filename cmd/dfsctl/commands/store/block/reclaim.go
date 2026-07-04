package block

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// reclaimCmd deletes class-1 orphans (zero-ref block records) across all
// remote-backed shares. It is the first deleting stage after `reconcile`.
var reclaimCmd = &cobra.Command{
	Use:   "reclaim",
	Short: "Reclaim zero-ref block records (deletes; use --dry-run to preview)",
	Long: `Delete class-1 orphaned block records across every remote-backed share and
free their remote objects. A zero-ref record has no live locator and a zero live
chunk count — left behind by a crash between decrementing the count and deleting
the record. Such a record is terminally dead, so reclaiming it is safe with no
grace window.

This DELETES. Run 'dfsctl store block reconcile' first to review what exists, or
pass --dry-run here to preview the exact set this command would delete without
deleting anything.

Examples:
  # Preview what would be reclaimed
  dfsctl store block reclaim --dry-run

  # Reclaim zero-ref records
  dfsctl store block reclaim

  # As JSON for scripting
  dfsctl store block reclaim -o json`,
	Args: cobra.NoArgs,
	RunE: runBlockStoreReclaim,
}

func init() {
	reclaimCmd.Flags().Bool("dry-run", false, "Report the records that would be reclaimed without deleting anything")
}

func runBlockStoreReclaim(cmd *cobra.Command, args []string) error {
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return err
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	report, err := client.BlockStoreReclaimZeroRef(&apiclient.BlockStoreReclaimZeroRefRequest{DryRun: dryRun})
	if err != nil {
		return fmt.Errorf("failed to reclaim zero-ref records: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, report)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, report)
	default:
		verb := "Reclaimed"
		if report.DryRun {
			verb = "Would reclaim"
		}
		pairs := [][2]string{
			{verb, classSummary(report.Reclaimed)},
			{"Block records scanned", fmt.Sprintf("%d", report.BlockRecordsScanned)},
			{"Errors", fmt.Sprintf("%d", report.Errors)},
			{"Dry run", fmt.Sprintf("%t", report.DryRun)},
		}
		if err := output.SimpleTable(os.Stdout, pairs); err != nil {
			return err
		}
		printSample(os.Stdout, verb, report.Reclaimed)
		return nil
	}
}
