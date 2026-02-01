package context

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

var deleteForce bool

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a context",
	Long: `Delete a server context.

This removes the saved configuration and credentials for the context.

Examples:
  # Delete context named "staging"
  dittofsctl context delete staging

  # Delete without confirmation
  dittofsctl context delete staging --force`,
	Args: cobra.ExactArgs(1),
	RunE: runContextDelete,
}

func init() {
	deleteCmd.Flags().BoolVarP(&deleteForce, "force", "f", false, "Skip confirmation")
}

func runContextDelete(cmd *cobra.Command, args []string) error {
	contextName := args[0]

	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	if _, err = store.GetContext(contextName); err != nil {
		if err == credentials.ErrContextNotFound {
			return fmt.Errorf("context '%s' not found", contextName)
		}
		return fmt.Errorf("failed to get context: %w", err)
	}

	return cmdutil.RunDeleteWithConfirmation("Context", contextName, deleteForce, func() error {
		return store.DeleteContext(contextName)
	})
}
