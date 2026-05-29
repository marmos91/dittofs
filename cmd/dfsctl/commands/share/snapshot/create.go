package snapshot

import (
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	createName     string
	createNoVerify bool
	createRetry    string
	createNoWait   bool
)

var createCmd = &cobra.Command{
	Use:   "create <share>",
	Short: "Create a snapshot of a share",
	Long: `Create a snapshot of a share.

By default the command blocks until the snapshot reaches a terminal
state (ready or failed). Use --no-wait to return immediately after the
snapshot is enqueued.

Examples:
  # Block until snapshot is ready
  dfsctl share snapshot create /archive --name weekly

  # Return immediately with the new snapshot ID
  dfsctl share snapshot create /archive --no-wait

  # Skip the remote-durability verify step
  dfsctl share snapshot create /archive --no-verify

  # Retry a failed previous snapshot
  dfsctl share snapshot create /archive --retry snap-prev123`,
	Args: cobra.ExactArgs(1),
	RunE: runCreate,
}

func init() {
	createCmd.Flags().StringVar(&createName, "name", "", "Human-readable name for the snapshot")
	createCmd.Flags().BoolVar(&createNoVerify, "no-verify", false, "Skip the remote-durability verify step")
	createCmd.Flags().StringVar(&createRetry, "retry", "", "Retry a previous failed snapshot by ID")
	createCmd.Flags().BoolVar(&createNoWait, "no-wait", false, "Return immediately instead of waiting for completion")
}

// noWaitRecord builds a Snapshot record from the 202 create response so the
// --no-wait JSON/YAML output carries an `id` field consistent with the
// blocking path (which emits the full Snapshot).
func noWaitRecord(resp *apiclient.CreateSnapshotResponse) apiclient.Snapshot {
	return apiclient.Snapshot{
		ID:    resp.SnapshotID,
		Share: resp.Share,
		State: "creating",
	}
}

func runCreate(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := getClient()
	if err != nil {
		return err
	}

	// Resolve a possibly-partial --retry id (e.g. the 8-char id from list)
	// to a full UUID before the server's exact-match retry lookup.
	retryOf := createRetry
	if retryOf != "" {
		retryOf, err = resolveSnapshotID(client, share, retryOf)
		if err != nil {
			return err
		}
	}

	resp, err := client.CreateSnapshot(share, apiclient.CreateSnapshotRequest{
		Name:     createName,
		NoVerify: createNoVerify,
		RetryOf:  retryOf,
	})
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	if createNoWait {
		// Emit a Snapshot-shaped record so the JSON/YAML shape matches the
		// blocking path: `jq '.id'` works regardless of --no-wait. The
		// server returns only {snapshot_id, share} on the 202, so synthesize
		// the known fields (the snapshot is necessarily 'creating' here).
		switch format {
		case output.FormatJSON:
			return output.PrintJSON(os.Stdout, noWaitRecord(resp))
		case output.FormatYAML:
			return output.PrintYAML(os.Stdout, noWaitRecord(resp))
		default:
			fmt.Printf("Snapshot %s queued on share %s (state: creating)\n", resp.SnapshotID, resp.Share)
		}
		return nil
	}

	if format == output.FormatTable {
		fmt.Printf("Snapshot %s queued on share %s (state: creating)\n", resp.SnapshotID, resp.Share)
	}

	ctx := cmd.Context()
	snap, err := client.WaitForSnapshot(ctx, share, resp.SnapshotID, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("wait for snapshot: %w", err)
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, snap)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, snap)
	default:
		switch snap.State {
		case "ready":
			fmt.Printf("Snapshot %s -> ready\n", snap.ID)
			return nil
		case "failed":
			msg := snap.Error
			if msg == "" {
				msg = "(no error message)"
			}
			fmt.Fprintf(os.Stderr, "Snapshot %s failed: %s\n", snap.ID, msg)
			return fmt.Errorf("snapshot failed")
		default:
			fmt.Printf("Snapshot %s in state %q\n", snap.ID, snap.State)
			return nil
		}
	}
}
