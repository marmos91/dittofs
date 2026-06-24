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
	Long: `Wait for all in-flight block store uploads to complete across every share.

The command blocks until the server confirms that no blocks are queued for remote upload, or until the server-side timeout (5 minutes) is reached. Use this before running benchmarks or taking snapshots to ensure a clean data boundary.

Examples:
  # Block until all pending uploads are flushed
  dfsctl system drain-uploads

  # Get drain result as JSON (includes duration)
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
