// Package blockstore implements offline block-store migration commands
// for dfsctl. The migration tool re-chunks legacy {payloadID}/block-{idx}
// keys into the v0.15 CAS layout per Phase 14 (D-A1..D-A20).
package blockstore

import "github.com/spf13/cobra"

// Cmd is the parent command for offline block-store migration tooling.
//
// Migration is offline-only: the tool refuses to run when the daemon
// owning the share is active (D-A5). The migration loop walks every file
// in the share, re-chunks legacy blocks via FastCDC, uploads CAS chunks
// dedup-aware via the metadata FileBlockStore, populates FileAttr.Blocks
// and FileAttr.ObjectID per file in a single metadata transaction, and
// journals progress under {share-data-dir}/.migration-state*.
//
// See docs/BLOCKSTORE_MIGRATION.md (added in Plan 14-07) for the full
// operator runbook.
var Cmd = &cobra.Command{
	Use:   "blockstore",
	Short: "Block-store migration and inspection",
	Long: `Block-store migration commands.

Offline tooling for converting v0.13 / v0.14 path-indexed legacy keys
({payloadID}/block-{idx}) into the v0.15 content-addressed CAS layout
(cas/{hh}/{hh}/{hash_hex}).

The migration is offline-only: stop the daemon owning the share before
invoking 'migrate'. See docs/BLOCKSTORE_MIGRATION.md for the runbook.

Examples:
  # Migrate a share (offline)
  dfsctl blockstore migrate --share myshare

  # Dry-run estimate
  dfsctl blockstore migrate --share myshare --dry-run`,
}

func init() {
	Cmd.AddCommand(migrateCmd)
	// Plan 14-06 (D-A16): operator-facing status surface, mirrors the
	// REST endpoint at /api/v1/blockstore/migrate/status.
	migrateCmd.AddCommand(migrateStatusCmd)
}
