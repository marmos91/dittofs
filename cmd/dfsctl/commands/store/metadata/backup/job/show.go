package job

import (
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
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
		rows = append(rows, []string{"Progress", renderProgressBar(j.Progress)})
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

// ---------------------------------------------------------------------------
// Format helpers — duplicated from backup/format.go because the `backup/job`
// sub-package can't import its parent `backup` (the parent imports
// `backup/job` to wire Cmd.AddCommand). The plan explicitly flags these as
// "vendor inline" helpers.
// ---------------------------------------------------------------------------

// shortULID returns the first 8 chars of a ULID followed by an ellipsis
// (U+2026 "…"). Mirrors backup/format.go:shortULID (D-26).
func shortULID(id string) string {
	const prefixLen = 8
	if len(id) <= prefixLen {
		return id
	}
	return id[:prefixLen] + "\u2026"
}

// timeAgo renders a duration relative to now such as "30s ago", "3m ago",
// "3h ago", or "2d ago". Mirrors backup/format.go:timeAgo.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// renderProgressBar renders a fixed-width 20-cell progress bar. Mirrors
// backup/format.go:renderProgressBar (D-47).
func renderProgressBar(pct int) string {
	const width = 20
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * width / 100
	return fmt.Sprintf("%d%%  [%s%s]", pct,
		strings.Repeat("\u2593", filled),
		strings.Repeat("\u2591", width-filled))
}
