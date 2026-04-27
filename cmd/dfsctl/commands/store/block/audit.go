package block

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// auditRefcountsCmd verifies INV-02 (∑ FileBlock.RefCount ==
// ∑ len(FileAttr.Blocks)) for the named share. Persists last-run
// summary at <localStore>/audit-state/last-inv02.json (Phase 12 D-36).
var auditRefcountsCmd = &cobra.Command{
	Use:   "audit-refcounts <share>",
	Short: "Verify FileBlock.RefCount matches FileAttr.Blocks references",
	Long: `Run the INV-02 reconciliation audit for the named share.

Computes ∑ FileBlock.RefCount and compares against ∑ len(FileAttr.Blocks)
across all files in the share. A non-zero delta indicates a refcount drift
that may block GC reclamation (a leaked block survives the grace window)
or signal a bug in the dedup short-circuit.

Persists last-run summary at <localStore>/audit-state/last-inv02.json
analogously to GC's last-run.json. Operator-invokable; no periodic
schedule in v0.15.0 (Phase 12 D-36).

Examples:
  dfsctl store block audit-refcounts myshare
  dfsctl store block audit-refcounts myshare -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runAuditRefcounts,
}

func runAuditRefcounts(_ *cobra.Command, args []string) error {
	share := args[0]
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	res, err := client.BlockStoreAuditRefcounts(share)
	if err != nil {
		return fmt.Errorf("failed to run INV-02 audit: %w", err)
	}
	if res == nil || res.Result == nil {
		return fmt.Errorf("INV-02 audit: server returned empty response")
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
		return printAuditTable(res)
	}
}

// printAuditTable renders the audit summary as a key/value table mirroring
// printGCStatsTable's shape. Highlights the Delta field — operators read
// that first to spot drift.
func printAuditTable(res *apiclient.BlockStoreAuditResult) error {
	r := res.Result
	pairs := [][2]string{
		{"Share", r.Share},
		{"Started At", r.StartedAt.Format("2006-01-02T15:04:05Z07:00")},
		{"Completed At", r.CompletedAt.Format("2006-01-02T15:04:05Z07:00")},
		{"Duration", fmt.Sprintf("%dms", r.DurationMS)},
		{"Total Files", fmt.Sprintf("%d", r.TotalFiles)},
		{"Total Refs (Σ len(Blocks))", fmt.Sprintf("%d", r.TotalRefs)},
		{"Total RefCount (Σ RefCount)", fmt.Sprintf("%d", r.TotalRefCount)},
		{"Delta (refs - refcount)", fmt.Sprintf("%d", r.Delta)},
	}
	if err := output.SimpleTable(os.Stdout, pairs); err != nil {
		return err
	}
	if r.Delta != 0 {
		fmt.Println()
		fmt.Printf("INV-02 violation: delta=%d — refcount drift detected\n", r.Delta)
	}
	return nil
}
