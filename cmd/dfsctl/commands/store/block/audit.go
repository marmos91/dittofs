package block

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// auditRefcountsCmd verifies the CAS manifest-consistency invariant for
// the named share: every block referenced by a file's manifest
// (FileAttr.Blocks) must have a backing FileBlock row in the metadata
// store. Persists last-run summary at <localStore>/audit-state/last-inv02.json.
var auditRefcountsCmd = &cobra.Command{
	Use:   "audit-refcounts <share>",
	Short: "Verify every manifest block reference has a backing FileBlock row",
	Long: `Run the CAS manifest-consistency audit for the named share.

Walks every file in the share and checks that each block referenced by the
file's manifest (FileAttr.Blocks) has a backing FileBlock row in the
metadata store. A manifest reference with no backing row is a genuine
DANGLING reference — the file claims a chunk the store has no record of, so
a read would return zeros or fail (the silent-data-loss class). The
invariant is "dangling refs == 0"; a non-zero count is real corruption
worth alerting on, so the command exits non-zero (use it as
` + "`audit-refcounts <share> || alert`" + `).

The legacy per-hash RefCount metric (∑ FileBlock.RefCount) was removed:
RefCount is not maintained in the content-addressed-store model (CAS blocks
are written Pending and never transition to Remote), so that sum was
structurally always 0 and produced false-positive "delta" alarms.

Persists last-run summary at <localStore>/audit-state/last-inv02.json
analogously to GC's last-run.json. Operator-invokable; no periodic schedule.

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
		return fmt.Errorf("failed to run manifest-consistency audit: %w", err)
	}
	if res == nil || res.Result == nil {
		return fmt.Errorf("manifest-consistency audit: server returned empty response")
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}
	// Emit the audit body first so observability is identical across
	// formats, then surface a detected dangling reference as a non-zero exit
	// independently of the output format. A non-zero Delta is a real
	// violation (a manifest ref with no backing FileBlock row — silent data
	// loss) — `audit-refcounts || alert` must fire in -o json/-o yaml too,
	// not only the table branch.
	switch format {
	case output.FormatJSON:
		if err := output.PrintJSON(os.Stdout, res); err != nil {
			return err
		}
	case output.FormatYAML:
		if err := output.PrintYAML(os.Stdout, res); err != nil {
			return err
		}
	default:
		if err := printAuditTable(res); err != nil {
			return err
		}
	}

	if res.Result.Delta != 0 {
		return fmt.Errorf("manifest-consistency violation: dangling refs delta=%d", res.Result.Delta)
	}
	return nil
}

// printAuditTable renders the audit summary as a key/value table mirroring
// printGCStatsTable's shape. Highlights the Dangling refs field — operators
// read that first to spot silent-data-loss corruption.
func printAuditTable(res *apiclient.BlockStoreAuditResult) error {
	r := res.Result
	pairs := [][2]string{
		{"Share", r.Share},
		{"Started At", r.StartedAt.Format("2006-01-02T15:04:05Z07:00")},
		{"Completed At", r.CompletedAt.Format("2006-01-02T15:04:05Z07:00")},
		{"Duration", fmt.Sprintf("%dms", r.DurationMS)},
		{"Total Files", fmt.Sprintf("%d", r.TotalFiles)},
		{"Manifest Refs (Σ len(Blocks))", fmt.Sprintf("%d", r.TotalRefs)},
		{"Backed by FileBlock row", fmt.Sprintf("%d", r.BackedRefs)},
		{"Dangling refs (missing row)", fmt.Sprintf("%d", r.DanglingRefs)},
	}
	if err := output.SimpleTable(os.Stdout, pairs); err != nil {
		return err
	}
	if r.Delta != 0 {
		fmt.Println()
		fmt.Printf("manifest-consistency violation: %d dangling reference(s) — manifest blocks with no backing FileBlock row\n", r.DanglingRefs)
	}
	return nil
}
