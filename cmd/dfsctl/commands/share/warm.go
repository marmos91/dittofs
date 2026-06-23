package share

import (
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// Terminal warm-job states (mirror shares.WarmState*).
const (
	warmStateDone     = "done"
	warmStateFailed   = "failed"
	warmStateCanceled = "canceled"
)

var warmCmd = &cobra.Command{
	Use:   "warm <name>",
	Short: "Warm a share's local block cache",
	Long: `Proactively materialize a share's blocks onto the local disk tier.

Starts an asynchronous job that downloads every remote block of the share into
the local cache so subsequent reads are served locally. The command prints the
job id and exits; use --watch to poll until the job completes.

The share must have a remote tier configured. A pinned share with a bounded
local tier may fail with a disk-full error if its working set exceeds the tier.

Examples:
  # Start a warm job and exit
  dfsctl share warm /archive

  # Start and follow progress until done
  dfsctl share warm --watch /archive`,
	Args: cobra.ExactArgs(1),
	RunE: runShareWarm,
}

func init() {
	warmCmd.Flags().Bool("watch", false, "Poll the job until it reaches a terminal state")
}

func runShareWarm(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	shareName := args[0]
	watch, _ := cmd.Flags().GetBool("watch")

	jobID, err := client.StartShareWarm(shareName)
	if err != nil {
		return fmt.Errorf("failed to start warm job: %w", err)
	}

	if !watch {
		format, ferr := cmdutil.GetOutputFormatParsed()
		if ferr != nil {
			return ferr
		}
		switch format {
		case output.FormatJSON:
			return output.PrintJSON(os.Stdout, map[string]string{"job_id": jobID})
		case output.FormatYAML:
			return output.PrintYAML(os.Stdout, map[string]string{"job_id": jobID})
		default:
			fmt.Printf("Warm job started: %s\n", jobID)
		}
		return nil
	}

	return watchWarm(client, shareName, jobID)
}

// watchWarm polls the warm job every second, rendering progress until a
// terminal state. It returns a non-nil error on failed/canceled jobs so the
// process exits non-zero.
func watchWarm(client *apiclient.Client, shareName, jobID string) error {
	for {
		status, err := client.GetShareWarm(shareName, jobID)
		if err != nil {
			return fmt.Errorf("failed to poll warm job: %w", err)
		}

		fmt.Printf("\rfetched %d/%d blocks (%s)        ",
			status.BlocksDone, status.BlocksTotal,
			bytesize.ByteSize(status.BytesDone).String())

		switch status.State {
		case warmStateDone:
			fmt.Printf("\rfetched %d/%d blocks (%s) — done\n",
				status.BlocksDone, status.BlocksTotal,
				bytesize.ByteSize(status.BytesDone).String())
			return nil
		case warmStateFailed:
			fmt.Println()
			return fmt.Errorf("warm job failed: %s", status.Error)
		case warmStateCanceled:
			fmt.Println()
			return fmt.Errorf("warm job canceled: %s", status.Error)
		}

		time.Sleep(1 * time.Second)
	}
}
