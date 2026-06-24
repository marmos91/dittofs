package context

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

var renameCmd = &cobra.Command{
	Use:   "rename <old-name> <new-name>",
	Short: "Rename a context",
	Long: `Rename an existing server context to a new name.

All stored credentials and the active-context pointer are updated atomically; no re-authentication is needed. Use this after promoting a staging server to production, or simply to give a context a more descriptive name.

Examples:
  # Rename the "default" context to "production"
  dfsctl context rename default production

  # Rename a development context to something more descriptive
  dfsctl context rename dev local-dev`,
	Args: cobra.ExactArgs(2),
	RunE: runContextRename,
}

func runContextRename(cmd *cobra.Command, args []string) error {
	oldName := args[0]
	newName := args[1]

	// Load credential store
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Rename context
	if err := store.RenameContext(oldName, newName); err != nil {
		if err == credentials.ErrContextNotFound {
			return fmt.Errorf("context '%s' not found", oldName)
		}
		return fmt.Errorf("failed to rename context: %w", err)
	}

	fmt.Printf("Context renamed: %s -> %s\n", oldName, newName)
	return nil
}
