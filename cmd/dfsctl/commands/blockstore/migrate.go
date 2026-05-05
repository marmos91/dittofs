package blockstore

import (
	"fmt"

	"github.com/spf13/cobra"
)

// migrateCmd is the offline re-chunking entrypoint. Walks every file in
// the named share, re-chunks legacy blocks via FastCDC, uploads CAS
// chunks (dedup-aware), updates FileAttr.Blocks + FileAttr.ObjectID per
// file in a single metadata txn, and journals progress.
//
// Phase 14 D-A1..D-A5, D-A14. Offline only — refuses to run when the
// daemon owning the share is active (D-A5).
var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate a share's blocks from legacy {payloadID}/block-{idx} keys to v0.15 CAS layout",
	Long: `Migrate a share from the legacy v0.13/v0.14 path-indexed block layout
to the v0.15 content-addressed (CAS) layout.

The tool is OFFLINE — stop the daemon owning the share before invoking.
The migration is idempotent: a crash mid-share resumes from the journal
({share-data-dir}/.migration-state.jsonl). Plan 14 D-A1..D-A5, D-A14.

Examples:
  # Migrate a share
  dfsctl blockstore migrate --share myshare

  # Estimate uploads without writing
  dfsctl blockstore migrate --share myshare --dry-run

  # Cap aggregate upload bandwidth (Plan 14-04 wires this)
  dfsctl blockstore migrate --share myshare --bandwidth-limit 50MB

  # Override journal directory
  dfsctl blockstore migrate --share myshare --state-dir /tmp/mig-state`,
	Args: cobra.NoArgs,
	RunE: runMigrate,
}

func init() {
	migrateCmd.Flags().String("share", "", "Share name to migrate (required)")
	migrateCmd.Flags().Bool("dry-run", false, "Walk file list and report estimated upload bytes WITHOUT writing any data")
	migrateCmd.Flags().Int("parallel", 4, "Number of parallel migration workers (D-A10 default 4); honored by Plan 14-04")
	migrateCmd.Flags().String("bandwidth-limit", "", "Aggregate upload bandwidth ceiling (e.g., 50MB, 100MiB); honored by Plan 14-04")
	migrateCmd.Flags().String("state-dir", "", "Override journal/snapshot directory; defaults to {share-data-dir}/.migration-state")
	_ = migrateCmd.MarkFlagRequired("share")
}

// runMigrate is the cobra entrypoint. It performs the daemon-active
// probe (D-A5) and then dispatches to the migration loop.
func runMigrate(cmd *cobra.Command, args []string) error {
	share, _ := cmd.Flags().GetString("share")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	parallel, _ := cmd.Flags().GetInt("parallel")
	bandwidth, _ := cmd.Flags().GetString("bandwidth-limit")
	stateDir, _ := cmd.Flags().GetString("state-dir")

	if err := ensureDaemonOffline(cmd.Context(), share); err != nil {
		return fmt.Errorf("refusing to migrate share %q: %w", share, err)
	}

	bps, err := ParseBandwidthLimit(bandwidth)
	if err != nil {
		return fmt.Errorf("parse --bandwidth-limit: %w", err)
	}

	opts := migrateOptions{
		share:        share,
		dryRun:       dryRun,
		parallel:     parallel,
		bandwidthRaw: bandwidth,
		bandwidthBPS: bps,
		stateDir:     stateDir,
	}
	return runMigrateLoop(cmd.Context(), opts)
}
