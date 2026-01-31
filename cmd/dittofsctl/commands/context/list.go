package context

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configured contexts",
	Long: `List all configured server contexts.

Shows the context name, server URL, and username for each saved context.
The current context is marked with an asterisk (*).

Examples:
  # List contexts as table
  dittofsctl context list

  # List as JSON
  dittofsctl context list -o json`,
	RunE: runContextList,
}

// ContextInfo represents context information for output.
type ContextInfo struct {
	Name      string `json:"name" yaml:"name"`
	Current   bool   `json:"current" yaml:"current"`
	ServerURL string `json:"server_url" yaml:"server_url"`
	Username  string `json:"username,omitempty" yaml:"username,omitempty"`
	LoggedIn  bool   `json:"logged_in" yaml:"logged_in"`
}

// ContextList is a list of contexts for table rendering.
type ContextList []ContextInfo

// Headers implements TableRenderer.
func (cl ContextList) Headers() []string {
	return []string{"", "NAME", "SERVER", "USER", "LOGGED IN"}
}

// Rows implements TableRenderer.
func (cl ContextList) Rows() [][]string {
	rows := make([][]string, 0, len(cl))
	for _, c := range cl {
		current := ""
		if c.Current {
			current = "*"
		}
		rows = append(rows, []string{current, c.Name, c.ServerURL, c.Username, cmdutil.BoolToYesNo(c.LoggedIn)})
	}
	return rows
}

func runContextList(cmd *cobra.Command, args []string) error {
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	contextNames := store.ListContexts()
	currentContext := store.GetCurrentContextName()

	contexts := make(ContextList, 0, len(contextNames))
	for _, name := range contextNames {
		ctx, err := store.GetContext(name)
		if err != nil {
			continue
		}

		info := ContextInfo{
			Name:      name,
			Current:   name == currentContext,
			ServerURL: ctx.ServerURL,
			Username:  ctx.Username,
			LoggedIn:  ctx.AccessToken != "" && !ctx.IsExpired(),
		}
		contexts = append(contexts, info)
	}

	return cmdutil.PrintOutput(os.Stdout, contexts, len(contexts) == 0, "No contexts configured. Use 'dittofsctl login --server <url>' to create one.", contexts)
}
