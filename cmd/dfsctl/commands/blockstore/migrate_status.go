package blockstore

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// migrateStatusCmd is the operator-facing per-share migration progress
// surface. Reads the same MigrateStatusResponse the REST endpoint serves
// (D-A16) — server combines the journal aggregate (Plan 14-03) with the
// per-share BlockLayout flag (Plan 14-01) and the file count walked from
// the metadata store. Default output is a key-value table; -o json/yaml
// switch to machine-parseable formats.
//
// Phase 14 D-A16, MIG-01 + MIG-02. The CLI is admin-authenticated by
// virtue of going through cmdutil.GetAuthenticatedClient, which carries
// the operator's JWT — the REST handler enforces RequireAdmin.
var migrateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show migration progress for a share",
	Long: `Show migration progress for a share.

Combines the per-share .migration-state.jsonl journal (when a migration
ran or is running) with the share's BlockLayout flag from the metadata
store, returning a unified view of progress and current state.

Fields:
  Share              the share name
  BlockLayout        "legacy" or "cas-only"
  FilesTotal         total regular files in the share (-1 if walk timed out)
  FilesDone          files committed by the migration journal
  FilesSkipped       files skipped (already CAS-laid-out)
  BytesUploaded      sum of bytes PUT to remote across done files
  BytesDeduped       sum of bytes hit by GetByHash (skipped at upload)
  JournalPresent     whether the .migration-state.jsonl exists + non-empty
  SnapshotPresent    whether the rolling snapshot exists
  LastCommitAt       wall-clock of the last journal commit (RFC3339, UTC)

Examples:
  dfsctl blockstore migrate status --share myshare
  dfsctl blockstore migrate status --share myshare -o json
  dfsctl blockstore migrate status --share myshare -o yaml`,
	Args: cobra.NoArgs,
	RunE: runMigrateStatus,
}

func init() {
	migrateStatusCmd.Flags().String("share", "", "Share name to query (required)")
	_ = migrateStatusCmd.MarkFlagRequired("share")
}

// migrateStatusRenderer renders MigrateStatusResponse as a key-value
// table. Mirrors graceStatusRenderer's shape so per-resource status
// commands have a consistent operator UX.
type migrateStatusRenderer struct {
	resp *apiclient.MigrateStatusResponse
}

// Headers implements output.TableRenderer.
func (r migrateStatusRenderer) Headers() []string {
	return []string{"FIELD", "VALUE"}
}

// Rows implements output.TableRenderer. Order matches the field order in
// MigrateStatusResponse so operators see the same flow as the JSON dump.
func (r migrateStatusRenderer) Rows() [][]string {
	return [][]string{
		{"Share", r.resp.Share},
		{"BlockLayout", r.resp.BlockLayout},
		{"FilesTotal", fmt.Sprintf("%d", r.resp.FilesTotal)},
		{"FilesDone", fmt.Sprintf("%d", r.resp.FilesDone)},
		{"FilesSkipped", fmt.Sprintf("%d", r.resp.FilesSkipped)},
		{"BytesUploaded", fmt.Sprintf("%d", r.resp.BytesUploaded)},
		{"BytesDeduped", fmt.Sprintf("%d", r.resp.BytesDeduped)},
		{"JournalPresent", fmt.Sprintf("%t", r.resp.JournalPresent)},
		{"SnapshotPresent", fmt.Sprintf("%t", r.resp.SnapshotPresent)},
		{"LastCommitAt", r.resp.LastCommitAt},
	}
}

func runMigrateStatus(cmd *cobra.Command, _ []string) error {
	share, _ := cmd.Flags().GetString("share")

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	resp, err := client.MigrateStatus(share)
	if err != nil {
		// Translate 404 → friendlier message; everything else bubbles up.
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) && apiErr.IsNotFound() {
			return fmt.Errorf("share %q not found", share)
		}
		return fmt.Errorf("get migration status: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}
	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, resp)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, resp)
	default:
		return output.PrintTable(os.Stdout, migrateStatusRenderer{resp: resp})
	}
}
