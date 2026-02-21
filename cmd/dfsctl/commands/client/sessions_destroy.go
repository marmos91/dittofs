package client

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/spf13/cobra"
)

var forceDestroySession bool

var sessionsDestroyCmd = &cobra.Command{
	Use:   "destroy <client-id> <session-id>",
	Short: "Force-destroy a session",
	Long: `Force-destroy an NFSv4.1 session by client ID and session ID.

This will forcefully tear down the session, bypassing in-flight request checks.
Use with caution -- the NFS client may experience errors.

Examples:
  # Destroy a session (with confirmation prompt)
  dfsctl client sessions destroy 0000000100000001 a1b2c3d4e5f6a7b8...

  # Destroy without confirmation
  dfsctl client sessions destroy 0000000100000001 a1b2c3d4e5f6a7b8... --force`,
	Args: cobra.ExactArgs(2),
	RunE: runSessionsDestroy,
}

func init() {
	sessionsDestroyCmd.Flags().BoolVarP(&forceDestroySession, "force", "f", false, "Skip confirmation prompt")
}

func runSessionsDestroy(cmd *cobra.Command, args []string) error {
	clientID := args[0]
	sessionID := args[1]

	confirmed, err := prompt.ConfirmWithForce(
		fmt.Sprintf("Force-destroy session %s for client %s?", sessionID, clientID),
		forceDestroySession,
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

	if err := client.ForceDestroySession(clientID, sessionID); err != nil {
		return fmt.Errorf("failed to destroy session: %w", err)
	}

	cmdutil.PrintSuccess(fmt.Sprintf("Session %s destroyed", sessionID))
	return nil
}
