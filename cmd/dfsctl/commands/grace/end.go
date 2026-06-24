package grace

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var endCmd = &cobra.Command{
	Use:   "end",
	Short: "Force-end the grace period",
	Long: `Force-end the NFSv4 grace period immediately.

This admin-only command terminates the grace period before it expires
naturally, allowing clients to create new state (open files, locks)
without waiting. Use it to accelerate recovery after a confirmed server
restart in development environments, or when all expected clients have
already reclaimed their state.

Examples:
  # Force-end the grace period
  dfsctl grace end

  # Verify the period has ended after forcing it
  dfsctl grace status`,
	RunE: runGraceEnd,
}

func runGraceEnd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	if err := client.ForceEndGrace(); err != nil {
		return fmt.Errorf("failed to end grace period: %w", err)
	}

	cmdutil.PrintSuccess("Grace period ended successfully")
	return nil
}
