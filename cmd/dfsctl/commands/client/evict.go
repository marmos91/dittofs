package client

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/spf13/cobra"
)

var forceDisconnect bool

var disconnectCmd = &cobra.Command{
	Use:     "disconnect <client-id>",
	Aliases: []string{"evict"},
	Short:   "Disconnect a client",
	Long: `Disconnect a connected client by its ID.

This performs protocol-specific teardown: for NFS clients it closes the TCP
connection and triggers state revocation; for SMB clients it triggers session
cleanup. Use with caution.

Examples:
  # Disconnect a client (with confirmation prompt)
  dfsctl client disconnect nfs-42

  # Disconnect without confirmation
  dfsctl client disconnect nfs-42 --force`,
	Args: cobra.ExactArgs(1),
	RunE: runDisconnect,
}

func init() {
	disconnectCmd.Flags().BoolVarP(&forceDisconnect, "force", "f", false, "Skip confirmation prompt")
}

func runDisconnect(cmd *cobra.Command, args []string) error {
	clientID := args[0]

	// Confirm before disconnection.
	confirmed, err := prompt.ConfirmWithForce(
		fmt.Sprintf("Disconnect client %s? This will close the connection and trigger cleanup.", clientID),
		forceDisconnect,
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

	if err := client.DisconnectClient(clientID); err != nil {
		return fmt.Errorf("failed to disconnect client: %w", err)
	}

	cmdutil.PrintSuccess(fmt.Sprintf("Client %s disconnected", clientID))
	return nil
}
