package settings

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var setCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a setting value",
	Long: `Set the value of a server setting.

Examples:
  # Set logging level
  dittofsctl settings set logging.level DEBUG

  # Set a numeric value
  dittofsctl settings set server.port 8080`,
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
		printer := output.NewPrinter(os.Stdout, format, !cmdutil.IsColorDisabled())
		printer.Success(fmt.Sprintf("Setting '%s' updated to '%s'", setting.Key, setting.Value))
	}

	return nil
}
