package context

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

var useCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Switch to a different context",
	Long: `Switch to a different server context.

This changes the active context used for subsequent commands.

Examples:
  # Switch to context named "production"
  dittofsctl context use production`,
	Args: cobra.ExactArgs(1),
	RunE: runContextUse,
}

func runContextUse(cmd *cobra.Command, args []string) error {
	contextName := args[0]

	// Load credential store
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Switch context
	if err := store.UseContext(contextName); err != nil {
		if err == credentials.ErrContextNotFound {
			return fmt.Errorf("context '%s' not found\n\n"+
				"List available contexts:\n"+
				"  dittofsctl context list", contextName)
		}
		return fmt.Errorf("failed to switch context: %w", err)
	}

	fmt.Printf("Switched to context: %s\n", contextName)
	return nil
}
