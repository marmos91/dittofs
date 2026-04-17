package job

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/backupfmt"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <job-id>",
	Short: "Show backup/restore job detail",
	Long: `Show detail for a single backup or restore job (D-47).

Table mode renders grouped FIELD | VALUE sections plus a progress bar when
the job is still running. JSON/YAML passes the flat BackupJob through
without the derived Duration or bar.
`,
	Args: cobra.ExactArgs(2), // <store-name> <job-id>
	RunE: runShow,
}

// BackupJobDetail is the grouped-section TableRenderer for D-47.
type BackupJobDetail struct {
	job *apiclient.BackupJob
}

func (d BackupJobDetail) Headers() []string { return []string{"FIELD", "VALUE"} }
func (d BackupJobDetail) Rows() [][]string {
	j := d.job
	rows := [][]string{
		{"ID", j.ID},
		{"Kind", j.Kind},
		{"Repo", j.RepoID},
	}
	if j.StartedAt != nil {
		rows = append(rows, []string{"Started", j.StartedAt.Format("2006-01-02 15:04:05 MST")})
	} else {
		rows = append(rows, []string{"Started", "-"})
	}
	if j.FinishedAt != nil {
		rows = append(rows, []string{"Finished", j.FinishedAt.Format("2006-01-02 15:04:05 MST")})
	} else {
		rows = append(rows, []string{"Finished", "-"})
	}
	rows = append(rows, []string{"Duration", durationOf(j)})
	rows = append(rows, []string{"Status", j.Status})
	// Progress bar only when running (D-47).
	if j.Status == "running" {
		rows = append(rows, []string{"Progress", backupfmt.RenderProgressBar(j.Progress)})
	}
	if j.Error != "" {
		rows = append(rows, []string{"Error", j.Error})
	}
	return rows
}

func runShow(cmd *cobra.Command, args []string) error {
	storeName, jobID := args[0], args[1]

	client, err := clientFactory()
	if err != nil {
		return err
	}

	job, err := client.GetBackupJob(storeName, jobID)
	if err != nil {
		return fmt.Errorf("failed to get backup job: %w", err)
	}

	format, fmtErr := cmdutil.GetOutputFormatParsed()
	if fmtErr != nil {
		format = output.FormatTable
	}
	if format != output.FormatTable {
		return cmdutil.PrintResource(stdoutOut, job, nil)
	}
	return output.PrintTable(stdoutOut, BackupJobDetail{job: job})
}

// durationOf returns the job's started->finished span, falling back to
// "start -> now" while the job is still running or "-" when start is
// unknown.
func durationOf(j *apiclient.BackupJob) string {
	if j == nil || j.StartedAt == nil {
		return "-"
	}
	endpoint := time.Now()
	if j.FinishedAt != nil {
		endpoint = *j.FinishedAt
	}
	d := endpoint.Sub(*j.StartedAt)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
