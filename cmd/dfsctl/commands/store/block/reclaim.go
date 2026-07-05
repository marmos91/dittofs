package block

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// reclaimCmd deletes orphaned block storage (zero-ref + leaked records and
// record-less remote objects) across all remote-backed shares. It is the deleting
// counterpart to `reconcile`.
var reclaimCmd = &cobra.Command{
	Use:   "reclaim",
	Short: "Reclaim orphaned block storage (deletes; use --dry-run to preview)",
	Long: `Delete orphaned block storage across every remote-backed share:

  - zero-ref records: no live locator and a zero live chunk count (a crash
    between decrementing the count and deleting the record);
  - leaked records: no live locator but a stale non-zero count, left behind
    when a block re-carve moved the hash without decrementing the old block;
  - record-less remote objects: an uploaded block with no backing record,
    older than the grace window (a commit that never landed).

A record with no live locator is terminally dead — block IDs are never reused —
so reclaiming records needs no grace window. Only record-less objects use one, to
spare an upload whose commit may still be in flight.

This DELETES. Run 'dfsctl store block reconcile' first to review what exists, or
pass --dry-run here to preview the exact set this command would delete without
deleting anything.

Examples:
  # Preview what would be reclaimed
  dfsctl store block reclaim --dry-run

  # Reclaim orphaned block storage
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

	report, err := client.BlockStoreReclaim(&apiclient.BlockStoreReclaimRequest{DryRun: dryRun})
	if err != nil {
		return fmt.Errorf("failed to reclaim orphaned block storage: %w", err)
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
			{verb + " zero-ref records", classSummary(report.Reclaimed)},
			{verb + " leaked records", classSummary(report.LeakedReclaimed)},
			{verb + " orphan objects", classSummary(report.OrphanObjectsReclaimed)},
			{"Block records scanned", fmt.Sprintf("%d", report.BlockRecordsScanned)},
			{"Remote objects scanned", fmt.Sprintf("%d", report.RemoteObjectsScanned)},
			{"Errors", fmt.Sprintf("%d", report.Errors)},
			{"Dry run", fmt.Sprintf("%t", report.DryRun)},
		}
		if err := output.SimpleTable(os.Stdout, pairs); err != nil {
			return err
		}
		printSample(os.Stdout, verb+" zero-ref", report.Reclaimed)
		printSample(os.Stdout, verb+" leaked", report.LeakedReclaimed)
		printSample(os.Stdout, verb+" orphan object", report.OrphanObjectsReclaimed)
		return nil
	}
}
