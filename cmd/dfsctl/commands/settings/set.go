package settings

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var setCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a setting value",
	Long: `Set the value of a server setting identified by its dot-separated key.

The change is applied immediately at runtime without a server restart. Use 'settings list' to discover available keys and their expected value types.

Examples:
  # Switch the server to DEBUG logging immediately
  dfsctl settings set logging.level DEBUG

  # Reset logging to the default level
  dfsctl settings set logging.level INFO`,
	Args: cobra.ExactArgs(2),
	RunE: runSet,
}

func runSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	setting, err := client.SetSetting(key, value)
	if err != nil {
		return fmt.Errorf("failed to set setting: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, setting, fmt.Sprintf("Setting '%s' updated to '%s'", setting.Key, setting.Value))
}
