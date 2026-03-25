package block

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
	Short: "Check block store health",
	Long: `Perform a health check on a block store configuration.

For local filesystem stores, checks if the path exists and is writable.
For local memory stores, always reports healthy.
For remote S3 stores, performs a HeadBucket call to verify connectivity.
For remote memory stores, always reports healthy.

Examples:
  # Check health of a local block store
  dfsctl store block health --kind local --name fs-cache

  # Check health of a remote block store
  dfsctl store block health --kind remote --name s3-store

  # Output as JSON
  dfsctl store block health --kind remote --name s3-store -o json`,
	RunE: runBlockStoreHealth,
}

func init() {
	healthCmd.Flags().String("kind", "", "Block store kind: local or remote (required)")
	healthCmd.Flags().String("name", "", "Block store name (required)")
	_ = healthCmd.MarkFlagRequired("kind")
	_ = healthCmd.MarkFlagRequired("name")
}

func runBlockStoreHealth(cmd *cobra.Command, _ []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	kind, _ := cmd.Flags().GetString("kind")
	name, _ := cmd.Flags().GetString("name")

	if kind != "local" && kind != "remote" {
		return fmt.Errorf("invalid kind %q: must be 'local' or 'remote'", kind)
	}

	resp, err := client.BlockStoreHealth(kind, name)
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
		if err := printBlockStoreHealthTable(resp); err != nil {
			return err
		}
	}

	if !resp.Healthy {
		return fmt.Errorf("store is unhealthy: %s", resp.Details)
	}
	return nil
}

func printBlockStoreHealthTable(resp *apiclient.BlockStoreHealthResult) error {
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
