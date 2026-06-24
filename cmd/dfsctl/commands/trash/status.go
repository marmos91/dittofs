package trash

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/cli/output"
)

// statusCmd prints the recycle-bin roll-up for a share.
var statusCmd = &cobra.Command{
	Use:   "status <share>",
	Short: "Show recycle-bin status for a share",
	Long: `Print a summary of a share's recycle bin: whether trash is enabled, the number of recycled entries, their combined size, and the timestamp of the oldest deletion.

Use this command for a quick health check before deciding whether to empty the bin or restore items. Pass -o json for machine-readable output.

Examples:
  # Show recycle bin status as a summary table
  dfsctl trash status myshare

  # Get status as JSON for scripting
  dfsctl trash status myshare -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runTrashStatus,
}

func runTrashStatus(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	status, err := client.TrashStatus(share)
	if err != nil {
		return fmt.Errorf("failed to read trash status: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, status)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, status)
	default:
		oldest := "-"
		if status.Oldest != nil {
			oldest = status.Oldest.Format("2006-01-02T15:04:05Z07:00")
		}
		pairs := [][2]string{
			{"Enabled", fmt.Sprintf("%v", status.Enabled)},
			{"Items", fmt.Sprintf("%d", status.ItemCount)},
			{"Total Size", bytesize.ByteSize(status.TotalBytes).String()},
			{"Oldest", oldest},
		}
		return output.SimpleTable(os.Stdout, pairs)
	}
}
