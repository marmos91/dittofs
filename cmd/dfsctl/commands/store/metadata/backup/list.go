package backup

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/backupfmt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listRepo string

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List backup records for a metadata store",
	Long: `List backup records (D-26).

Table columns: ID | CREATED | SIZE | STATUS | REPO | PINNED.
ID is short-prefixed (first 8 chars + ellipsis); CREATED renders as a relative
time; SIZE is human-readable. JSON/YAML output surfaces the full record shape.
`,
	Args: cobra.ExactArgs(1), // <store-name>
	RunE: runList,
}

func init() {
	listCmd.Flags().StringVar(&listRepo, "repo", "",
		"Filter by backup repo name (required when >1 repo attached per D-24)")
}

// BackupRecordList is the TableRenderer for `backup list` (D-26).
type BackupRecordList []apiclient.BackupRecord

func (bl BackupRecordList) Headers() []string {
	return []string{"ID", "CREATED", "SIZE", "STATUS", "REPO", "PINNED"}
}

func (bl BackupRecordList) Rows() [][]string {
	rows := make([][]string, 0, len(bl))
	for _, r := range bl {
		pinned := "-"
		if r.Pinned {
			pinned = "yes"
		}
		rows = append(rows, []string{
			backupfmt.ShortULID(r.ID),
			backupfmt.TimeAgo(r.CreatedAt),
			humanSize(r.SizeBytes),
			r.Status,
			r.RepoID,
			pinned,
		})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	storeName := args[0]

	client, err := getClient()
	if err != nil {
		return err
	}

	records, err := client.ListBackupRecords(storeName, listRepo)
	if err != nil {
		return fmt.Errorf("failed to list backup records: %w", err)
	}

	hint := fmt.Sprintf("No backups yet. Run: dfsctl store metadata %s backup", storeName)
	if listRepo != "" {
		hint += " --repo " + listRepo
	}
	return cmdutil.PrintOutput(stdoutOut, records, len(records) == 0, hint, BackupRecordList(records))
}
