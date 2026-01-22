package context

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

var renameCmd = &cobra.Command{
	Use:   "rename <old-name> <new-name>",
	Short: "Rename a context",
	Long: `Rename an existing server context.

Examples:
  # Rename context from "default" to "production"
  dittofsctl context rename default production`,
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
