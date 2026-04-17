package backup

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var pinCmd = &cobra.Command{
	Use:   "pin <record-id>",
	Short: "Mark a backup record as pinned (retention-exempt)",
	Long: `Set Pinned=true on the given backup record (D-23).

Pinned records are skipped by the retention pruner (Phase 4 D-09). JSON/YAML
modes return the updated record; table mode prints a short success line.
`,
	Args: cobra.ExactArgs(2), // <store-name> <record-id>
	RunE: runPin,
}

func runPin(cmd *cobra.Command, args []string) error {
	storeName, recordID := args[0], args[1]

	client, err := getClient()
	if err != nil {
		return err
	}

	rec, err := client.SetBackupRecordPinned(storeName, recordID, true)
	if err != nil {
		return fmt.Errorf("failed to pin backup record: %w", err)
	}
	return cmdutil.PrintResourceWithSuccess(stdoutOut, rec,
		fmt.Sprintf("Record '%s' pinned", recordID))
}
