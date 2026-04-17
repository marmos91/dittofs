package job

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// Flag vars — package-level so tests can mutate them directly.
var (
	listStatus string
	listKind   string
	listRepo   string
	listLimit  int
)

// stdoutOut is the writer job list/show/cancel use for structured output.
// Overridable in tests; production = os.Stdout.
var stdoutOut io.Writer = os.Stdout

// stderrOut is the writer job verbs use for hints (next-step, errors).
// Overridable in tests.
var stderrOut io.Writer = os.Stderr

// clientFactory defaults to the cmdutil credential-store helper. Tests swap
// it to inject an httptest-backed *apiclient.Client without touching the
// real credential store.
var clientFactory = cmdutil.GetAuthenticatedClient

// Recognised enum values. Validated client-side BEFORE hitting the API so
// typos don't burn a network round-trip.
var (
	validStatuses = map[string]bool{
		"pending":     true,
		"running":     true,
		"succeeded":   true,
		"failed":      true,
		"interrupted": true,
	}
	validKinds = map[string]bool{
		"backup":  true,
		"restore": true,
	}
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List backup/restore job attempts",
	Long: `List backup and restore job attempts for a metadata store (D-42).

Filters are validated client-side before the HTTP round-trip so typos fail
fast. Default sort: newest first by StartedAt (server-side). --limit caps at
200 server-side; 0 means "server default" (50).

Examples:
  # List running backup jobs for a specific repo
  dfsctl store metadata fast-meta backup job list --status running --kind backup --repo daily-s3

  # Last 20 job attempts of any kind
  dfsctl store metadata fast-meta backup job list --limit 20`,
	Args: cobra.ExactArgs(1), // <store-name>
	RunE: runList,
}

func init() {
	listCmd.Flags().StringVar(&listStatus, "status", "",
		"Filter by status (pending|running|succeeded|failed|interrupted)")
	listCmd.Flags().StringVar(&listKind, "kind", "",
		"Filter by kind (backup|restore)")
	listCmd.Flags().StringVar(&listRepo, "repo", "",
		"Filter by backup repo name")
	listCmd.Flags().IntVar(&listLimit, "limit", 0,
		"Maximum rows to return (0 = server default 50; cap 200)")
}

// BackupJobList is the TableRenderer for `backup job list` (D-42).
type BackupJobList []apiclient.BackupJob

func (bl BackupJobList) Headers() []string {
	return []string{"JOB ID", "KIND", "REPO", "STATUS", "STARTED", "DURATION", "PROGRESS"}
}

func (bl BackupJobList) Rows() [][]string {
	rows := make([][]string, 0, len(bl))
	for _, j := range bl {
		started := "-"
		dur := "-"
		if j.StartedAt != nil {
			started = timeAgo(*j.StartedAt)
			endpoint := time.Now()
			if j.FinishedAt != nil {
				endpoint = *j.FinishedAt
			}
			d := endpoint.Sub(*j.StartedAt)
			if d < 0 {
				d = 0
			}
			dur = d.Round(time.Second).String()
		}
		rows = append(rows, []string{
			shortULID(j.ID),
			j.Kind,
			j.RepoID,
			j.Status,
			started,
			dur,
			fmt.Sprintf("%d%%", j.Progress),
		})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	storeName := args[0]

	if listStatus != "" && !validStatuses[listStatus] {
		return fmt.Errorf("invalid status %q (valid: pending, running, succeeded, failed, interrupted)", listStatus)
	}
	if listKind != "" && !validKinds[listKind] {
		return fmt.Errorf("invalid kind %q (valid: backup, restore)", listKind)
	}
	if listLimit < 0 {
		return fmt.Errorf("--limit must be non-negative (got %d)", listLimit)
	}

	client, err := clientFactory()
	if err != nil {
		return err
	}

	jobs, err := client.ListBackupJobs(storeName, apiclient.BackupJobFilter{
		Status:   listStatus,
		Kind:     listKind,
		RepoName: listRepo,
		Limit:    listLimit,
	})
	if err != nil {
		return fmt.Errorf("failed to list backup jobs: %w", err)
	}

	hint := fmt.Sprintf("No backup jobs found. Run: dfsctl store metadata %s backup", storeName)
	if listRepo != "" {
		hint += " --repo " + listRepo
	}
	return cmdutil.PrintOutput(stdoutOut, jobs, len(jobs) == 0, hint, BackupJobList(jobs))
}
