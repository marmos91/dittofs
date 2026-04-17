package backup

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var unpinCmd = &cobra.Command{
	Use:   "unpin <record-id>",
	Short: "Clear the pinned flag on a backup record",
	Long: `Set Pinned=false on the given backup record (D-23).

Unpinned records become eligible for retention pruning under Phase 4 D-09.
JSON/YAML modes return the updated record; table mode prints a short success
line.
`,
	Args: cobra.ExactArgs(2), // <store-name> <record-id>
	RunE: runUnpin,
}

func runUnpin(cmd *cobra.Command, args []string) error {
	storeName, recordID := args[0], args[1]

	client, err := getClient()
	if err != nil {
		return err
	}

	rec, err := client.SetBackupRecordPinned(storeName, recordID, false)
	if err != nil {
		return fmt.Errorf("failed to unpin backup record: %w", err)
	}
	return cmdutil.PrintResourceWithSuccess(stdoutOut, rec,
		fmt.Sprintf("Record '%s' unpinned", recordID))
}
