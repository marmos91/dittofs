package system

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var drainUploadsCmd = &cobra.Command{
	Use:   "drain-uploads",
	Short: "Wait for all pending uploads to complete",
	Long: `Wait for all in-flight block store uploads to complete across all files.

This is useful for benchmarking and testing to ensure clean boundaries
between workloads. The command blocks until all uploads are drained or
the server-side timeout (5 minutes) is reached.

Examples:
  # Drain all pending uploads
  dfsctl system drain-uploads

  # Output as JSON
  dfsctl system drain-uploads -o json`,
	RunE: runDrainUploads,
}

func runDrainUploads(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	resp, err := client.DrainUploads()
	if err != nil {
		return fmt.Errorf("failed to drain uploads: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, resp)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, resp)
	default:
		fmt.Printf("All uploads drained (took %s)\n", resp.Duration)
	}

	return nil
}
