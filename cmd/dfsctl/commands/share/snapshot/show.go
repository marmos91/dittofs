package snapshot

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <share> <id>",
	Short: "Show details of a snapshot",
	Args:  cobra.ExactArgs(2),
	RunE:  runShow,
}

func runShow(cmd *cobra.Command, args []string) error {
	share, id := args[0], args[1]

	client, err := getClient()
	if err != nil {
		return err
	}

	id, err = resolveSnapshotID(client, share, id)
	if err != nil {
		return err
	}

	snap, err := client.GetSnapshot(share, id)
	if err != nil {
		return fmt.Errorf("failed to get snapshot: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, snap)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, snap)
	default:
		pairs := [][2]string{
			{"ID", snap.ID},
			{"NAME", cmdutil.EmptyOr(snap.Name, "-")},
			{"SHARE", snap.Share},
			{"STATE", snap.State},
			{"REMOTE DURABLE", cmdutil.BoolToYesNo(snap.RemoteDurable)},
			{"MANIFEST COUNT", fmt.Sprintf("%d", snap.ManifestCount)},
			{"DUMP BYTES", bytesize.ByteSize(snap.DumpBytes).String()},
			{"RETRY OF", cmdutil.EmptyOr(snap.RetryOf, "-")},
			{"ERROR", cmdutil.EmptyOr(snap.Error, "-")},
			{"CREATED AT", snap.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")},
			{"UPDATED AT", snap.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")},
		}
		return output.SimpleTable(os.Stdout, pairs)
	}
}
