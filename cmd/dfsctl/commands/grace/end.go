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

This is an admin-only operation that immediately ends the grace period,
allowing new state-creating operations to proceed. Use this for fast
recovery in development and testing environments.

Examples:
  # Force-end the grace period
  dfsctl grace end`,
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
