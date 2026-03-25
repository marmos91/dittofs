package metadata

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var healthCmd = &cobra.Command{
	Use:   "health",
	Short: "Check metadata store health",
	Long: `Perform a health check on a metadata store.

If the store is loaded in the runtime, calls its native health check method.
Otherwise, reports that the store is not loaded.

Examples:
  # Check health of a metadata store
  dfsctl store metadata health --name fast-meta

  # Output as JSON
  dfsctl store metadata health --name fast-meta -o json`,
	RunE: runMetadataStoreHealth,
}

func init() {
	healthCmd.Flags().String("name", "", "Metadata store name (required)")
	_ = healthCmd.MarkFlagRequired("name")
}

func runMetadataStoreHealth(cmd *cobra.Command, _ []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	name, _ := cmd.Flags().GetString("name")

	resp, err := client.MetadataStoreHealth(name)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		if err := output.PrintJSON(os.Stdout, resp); err != nil {
			return err
		}
	case output.FormatYAML:
		if err := output.PrintYAML(os.Stdout, resp); err != nil {
			return err
		}
	default:
		if err := printMetadataStoreHealthTable(resp); err != nil {
			return err
		}
	}

	if !resp.Healthy {
		return fmt.Errorf("store is unhealthy: %s", resp.Details)
	}
	return nil
}

func printMetadataStoreHealthTable(resp *apiclient.MetadataStoreHealthResult) error {
	status := "HEALTHY"
	if !resp.Healthy {
		status = "UNHEALTHY"
	}

	pairs := [][2]string{
		{"Status", status},
		{"Latency", fmt.Sprintf("%d ms", resp.LatencyMs)},
		{"Checked At", resp.CheckedAt},
	}
	if resp.Details != "" {
		pairs = append(pairs, [2]string{"Details", resp.Details})
	}

	return output.SimpleTable(os.Stdout, pairs)
}
