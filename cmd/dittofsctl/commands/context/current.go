package context

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var currentOutput string

var currentCmd = &cobra.Command{
	Use:   "current",
	Short: "Show current context",
	Long: `Display information about the current active context.

Examples:
  # Show current context
  dittofsctl context current

  # Show as JSON
  dittofsctl context current --output json`,
	RunE: runContextCurrent,
}

func init() {
	currentCmd.Flags().StringVarP(&currentOutput, "output", "o", "table", "Output format (table|json|yaml)")
}

func runContextCurrent(cmd *cobra.Command, args []string) error {
	// Load credential store
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Get current context
	contextName := store.GetCurrentContextName()
	if contextName == "" {
		return fmt.Errorf("no current context set\n\n" +
			"Login to a server first:\n" +
			"  dittofsctl login --server http://localhost:8080")
	}

	ctx, err := store.GetContext(contextName)
	if err != nil {
		return fmt.Errorf("failed to get context: %w", err)
	}

	info := ContextInfo{
		Name:      contextName,
		Current:   true,
		ServerURL: ctx.ServerURL,
		Username:  ctx.Username,
		LoggedIn:  ctx.AccessToken != "" && !ctx.IsExpired(),
	}

	// Parse output format
	format, err := output.ParseFormat(currentOutput)
	if err != nil {
		return err
	}

	// Print output
	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, info)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, info)
	default:
		fmt.Printf("Current context: %s\n", contextName)
		fmt.Printf("  Server:    %s\n", ctx.ServerURL)
		fmt.Printf("  User:      %s\n", ctx.Username)
		if info.LoggedIn {
			fmt.Printf("  Status:    Logged in\n")
		} else {
			fmt.Printf("  Status:    Not logged in\n")
		}
	}

	return nil
}
