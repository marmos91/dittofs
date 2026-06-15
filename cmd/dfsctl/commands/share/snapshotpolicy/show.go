package snapshotpolicy

import (
	"fmt"

	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <share>",
	Short: "Show a share's snapshot policy",
	Args:  cobra.ExactArgs(1),
	RunE:  runShow,
}

func runShow(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := getClient()
	if err != nil {
		return err
	}

	policy, err := client.GetSnapshotPolicy(share)
	if err != nil {
		return fmt.Errorf("failed to get snapshot policy: %w", err)
	}
	return printPolicy(policy)
}
