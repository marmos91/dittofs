package context

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

var deleteForce bool

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a context",
	Long: `Delete a saved server context and its stored credentials.

The context's configuration and access token are removed from the local credential store. Use this to clean up after decommissioning a server or when a context was created by mistake.

Examples:
  # Delete the "staging" context with a confirmation prompt
  dfsctl context delete staging

  # Delete without the confirmation prompt (e.g. in a script)
  dfsctl context delete staging --force`,
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
