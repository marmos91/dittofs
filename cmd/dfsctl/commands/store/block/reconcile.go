package block

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// reconcileCmd reports orphaned block storage across all remote-backed shares
// WITHOUT deleting anything.
var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Report orphaned block storage (read-only; no deletes)",
	Long: `Scan every remote-backed share for orphaned block storage and print a
classified report. This is READ-ONLY: it deletes nothing, decrements nothing,
and changes no markers. Use it to review what the later reclaim stages would
act on before running them.

Four orphan classes are reported:

  Zero-ref records       Block records with no live locator and a zero live
                         chunk count — a crash between decrementing the count
                         and deleting the record.
  Leaked blocks          Block records with no live locator but a non-zero live
                         chunk count — a re-carve moved the hash onto a new
                         block without decrementing this one.
  Orphan remote objects  blocks/<id> objects with no backing record, older than
                         the grace window — the upload succeeded but the commit
                         failed. Objects within the grace window are preserved
                         (they may be freshly uploaded, commit pending).
  Stranded local chunks  Unsynced, local-durable chunks awaiting upload.

Each class reports an exact count plus a bounded sample of IDs (truncated is
flagged when the full set is larger than the sample).

Examples:
  # Report orphans as a table
  dfsctl store block reconcile

  # As JSON for scripting
  dfsctl store block reconcile -o json`,
	Args: cobra.NoArgs,
	RunE: runBlockStoreReconcile,
}

func runBlockStoreReconcile(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	report, err := client.BlockStoreReconcileReport()
	if err != nil {
		return fmt.Errorf("failed to fetch reconcile report: %w", err)
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
		pairs := [][2]string{
			{"Zero-ref records", classSummary(report.ZeroRefRecords)},
			{"Leaked blocks", classSummary(report.LeakedBlocks)},
			{"Orphan remote objects", classSummary(report.OrphanRemoteObjects)},
			{"Stranded local chunks", classSummary(report.StrandedLocalChunks)},
			{"Block records scanned", fmt.Sprintf("%d", report.BlockRecordsScanned)},
			{"Remote objects scanned", fmt.Sprintf("%d", report.RemoteObjectsScanned)},
			{"Grace period", report.GracePeriod.String()},
		}
		if err := output.SimpleTable(os.Stdout, pairs); err != nil {
			return err
		}
		printSample(os.Stdout, "Zero-ref records", report.ZeroRefRecords)
		printSample(os.Stdout, "Leaked blocks", report.LeakedBlocks)
		printSample(os.Stdout, "Orphan remote objects", report.OrphanRemoteObjects)
		printSample(os.Stdout, "Stranded local chunks", report.StrandedLocalChunks)
		return nil
	}
}

// classSummary renders one class's count + bytes for the summary table.
func classSummary(c engine.ReconcileClass) string {
	s := fmt.Sprintf("%d", c.Count)
	if c.Bytes > 0 {
		s += " (" + formatBytes(c.Bytes) + ")"
	}
	return s
}

// printSample lists a class's sampled IDs, noting truncation.
func printSample(w *os.File, label string, c engine.ReconcileClass) {
	if len(c.Sample) == 0 {
		return
	}
	suffix := ""
	if c.Truncated {
		suffix = fmt.Sprintf(" (showing %d of %d)", len(c.Sample), c.Count)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "%s sample%s:\n", label, suffix)
	_, _ = fmt.Fprintf(w, "  %s\n", strings.Join(c.Sample, "\n  "))
}
