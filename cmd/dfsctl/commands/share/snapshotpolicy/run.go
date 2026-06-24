package snapshotpolicy

import (
	"fmt"

	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <share>",
	Short: "Trigger a share's snapshot policy now (manual override)",
	Long: `Run a share's snapshot policy immediately, ignoring its interval.

This creates a scheduled snapshot now, advances the policy's run clock, and
prunes per the retention bounds (keep-last / TTL). It is useful to take an
out-of-band snapshot before a maintenance window without changing the schedule
or creating a permanent manual snapshot that will never be pruned.

Examples:
  # Trigger the policy for a share immediately
  dfsctl share snapshot-policy run /archive

  # Trigger and then confirm the snapshot was created
  dfsctl share snapshot-policy run /archive && dfsctl share snapshot list /archive --state ready`,
	Args: cobra.ExactArgs(1),
	RunE: runRun,
}

func runRun(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := getClient()
	if err != nil {
		return err
	}

	resp, err := client.RunSnapshotPolicy(share)
	if err != nil {
		return fmt.Errorf("failed to run snapshot policy: %w", err)
	}
	fmt.Printf("Snapshot %s queued on share %s (state: creating)\n", resp.SnapshotID, resp.Share)
	return nil
}
