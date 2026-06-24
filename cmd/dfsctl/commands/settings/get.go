package settings

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var getCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a setting value",
	Long: `Get the current value of a single server setting by its dot-separated key.

The default output prints key = value to stdout. Pass -o json or -o yaml to get a structured response including the setting description, useful for automation.

Examples:
  # Print the current logging level
  dfsctl settings get logging.level

  # Get the setting as JSON for scripting
  dfsctl settings get logging.level -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runGet,
}

func runGet(cmd *cobra.Command, args []string) error {
	key := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	setting, err := client.GetSetting(key)
	if err != nil {
		return fmt.Errorf("failed to get setting: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, setting)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, setting)
	default:
		fmt.Printf("%s = %s\n", setting.Key, setting.Value)
	}

	return nil
}
