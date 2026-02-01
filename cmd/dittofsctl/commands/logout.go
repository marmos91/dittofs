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

This removes the access and refresh tokens but keeps the server URL
and context configuration for easy re-login.

Examples:
  # Logout from current context
  dittofsctl logout`,
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
