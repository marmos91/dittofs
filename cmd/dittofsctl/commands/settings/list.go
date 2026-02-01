package settings

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all settings",
	Long: `List all server settings.

Examples:
  # List as table
  dittofsctl settings list

  # List as JSON
  dittofsctl settings list -o json`,
	RunE: runList,
}

// SettingsList is a list of settings for table rendering.
type SettingsList []apiclient.Setting

// Headers implements TableRenderer.
func (sl SettingsList) Headers() []string {
	return []string{"KEY", "VALUE", "DESCRIPTION"}
}

// Rows implements TableRenderer.
func (sl SettingsList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		desc := s.Description
		if desc == "" {
			desc = "-"
		}
		rows = append(rows, []string{s.Key, s.Value, desc})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	settings, err := client.ListSettings()
	if err != nil {
		return fmt.Errorf("failed to list settings: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, settings, len(settings) == 0, "No settings found.", SettingsList(settings))
}
