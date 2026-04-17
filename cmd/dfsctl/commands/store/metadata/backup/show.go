package backup

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <record-id>",
	Short: "Show a backup record",
	Long: `Display the full detail for a single backup record (D-48).

Table mode renders grouped FIELD | VALUE rows; JSON/YAML passes the full
BackupRecord through untouched.
`,
	Args: cobra.ExactArgs(2), // <store-name> <record-id>
	RunE: runShow,
}

// BackupRecordDetail renders a single record as FIELD | VALUE rows.
type BackupRecordDetail struct {
	rec *apiclient.BackupRecord
}

func (d BackupRecordDetail) Headers() []string { return []string{"FIELD", "VALUE"} }
func (d BackupRecordDetail) Rows() [][]string {
	r := d.rec
	pinned := "no"
	if r.Pinned {
		pinned = "yes"
	}
	rows := [][]string{
		{"ID", r.ID},
		{"Status", r.Status},
		{"Repo", r.RepoID},
		{"Created", r.CreatedAt.Format("2006-01-02 15:04:05 MST")},
		{"Size", humanSize(r.SizeBytes)},
		{"SHA256", r.SHA256},
		{"StoreID", r.StoreID},
		{"Pinned", pinned},
		{"ManifestPath", r.ManifestPath},
	}
	if r.Error != "" {
		rows = append(rows, []string{"Error", r.Error})
	}
	return rows
}

func runShow(cmd *cobra.Command, args []string) error {
	storeName, recordID := args[0], args[1]

	client, err := getClient()
	if err != nil {
		return err
	}

	rec, err := client.GetBackupRecord(storeName, recordID)
	if err != nil {
		return fmt.Errorf("failed to get backup record: %w", err)
	}

	format := parseFormat()
	if format != output.FormatTable {
		return cmdutil.PrintResource(stdoutOut, rec, nil)
	}
	return output.PrintTable(stdoutOut, BackupRecordDetail{rec: rec})
}
