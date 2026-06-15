package snapshotpolicy

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all snapshot policies",
	Args:  cobra.NoArgs,
	RunE:  runList,
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := getClient()
	if err != nil {
		return err
	}

	policies, err := client.ListSnapshotPolicies()
	if err != nil {
		return fmt.Errorf("failed to list snapshot policies: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, policies)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, policies)
	default:
		if len(policies) == 0 {
			fmt.Println("No snapshot policies configured.")
			return nil
		}
		rows := make(PolicyList, 0, len(policies))
		for _, p := range policies {
			rows = append(rows, toRow(p))
		}
		return output.PrintTable(os.Stdout, rows)
	}
}
