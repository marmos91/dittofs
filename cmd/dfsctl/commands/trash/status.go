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
	Long: `Print the recycle-bin roll-up for a share: whether trash is enabled,
how many recycled roots it holds, their total size, and the oldest deletion.

Examples:
  dfsctl trash status myshare
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
