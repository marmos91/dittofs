package commands

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear stored credentials",
	Long: `Clear stored credentials for the current context.

Removes the access and refresh tokens from the active context in
~/.config/dfsctl/config.json but preserves the server URL and context name so
you can re-authenticate quickly with dfsctl login. To switch between contexts
or remove a context entirely, use the dfsctl context subcommand.

Examples:
  # Logout from the current context (clears tokens, keeps server URL)
  dfsctl logout

  # Logout then immediately log back in as a different user
  dfsctl logout && dfsctl login --username operator`,
	RunE: runLogout,
}

func runLogout(cmd *cobra.Command, args []string) error {
	// Load credential store
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Check if there's a current context
	contextName := store.GetCurrentContextName()
	if contextName == "" {
		return fmt.Errorf("not logged in - no current context")
	}

	// Clear credentials for current context
	if err := store.ClearCurrentContext(); err != nil {
		return fmt.Errorf("failed to clear credentials: %w", err)
	}

	fmt.Printf("Logged out from context: %s\n", contextName)
	return nil
}
