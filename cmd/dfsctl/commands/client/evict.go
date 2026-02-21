package client

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/spf13/cobra"
)

var forceEvict bool

var evictCmd = &cobra.Command{
	Use:   "evict <client-id>",
	Short: "Evict an NFS client",
	Long: `Evict a connected NFS client by its hex-encoded client ID.

This will forcefully disconnect the client and clean up all associated
state (open files, locks, delegations). Use with caution.

Examples:
  # Evict a client (with confirmation prompt)
  dfsctl client evict 0000000100000001

  # Evict without confirmation
  dfsctl client evict 0000000100000001 --force`,
	Args: cobra.ExactArgs(1),
	RunE: runEvict,
}

func init() {
	evictCmd.Flags().BoolVarP(&forceEvict, "force", "f", false, "Skip confirmation prompt")
}

func runEvict(cmd *cobra.Command, args []string) error {
	clientID := args[0]

	// Confirm before eviction
	confirmed, err := prompt.ConfirmWithForce(
		fmt.Sprintf("Evict client %s? This will disconnect the client.", clientID),
		forceEvict,
	)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if !confirmed {
		fmt.Println("Aborted.")
		return nil
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	if err := client.EvictClient(clientID); err != nil {
		return fmt.Errorf("failed to evict client: %w", err)
	}

	cmdutil.PrintSuccess(fmt.Sprintf("Client %s evicted", clientID))
	return nil
}
