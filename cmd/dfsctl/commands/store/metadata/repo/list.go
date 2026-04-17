package repo

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list <store-name>",
	Short: "List backup repos attached to a metadata store",
	Long: `List all backup repos attached to the named metadata store.

Columns (default table): NAME | KIND | SCHEDULE | RETENTION | ENCRYPTED

RETENTION renders as 'count=N age=Dd', 'count=N', 'age=Dd', or '-'.

Examples:
  # List as table
  dfsctl store metadata fast-meta repo list

  # List as JSON (full BackupRepo shape)
  dfsctl store metadata fast-meta repo list -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runList,
}

// RepoList is a slice of BackupRepos that implements output.TableRenderer
// with the D-20 column layout.
type RepoList []apiclient.BackupRepo

// Headers implements TableRenderer.
func (rl RepoList) Headers() []string {
	return []string{"NAME", "KIND", "SCHEDULE", "RETENTION", "ENCRYPTED"}
}

// Rows implements TableRenderer. Follows D-20 rendering rules.
func (rl RepoList) Rows() [][]string {
	rows := make([][]string, 0, len(rl))
	for _, r := range rl {
		schedule := "-"
		if r.Schedule != nil && *r.Schedule != "" {
			schedule = *r.Schedule
		}
		rows = append(rows, []string{
			r.Name,
			r.Kind,
			schedule,
			renderRetention(r),
			renderEncrypted(r),
		})
	}
	return rows
}

// renderRetention follows D-20: "count=7 age=14d" / "count=7" / "age=14d" / "-".
func renderRetention(r apiclient.BackupRepo) string {
	parts := make([]string, 0, 2)
	if r.KeepCount != nil && *r.KeepCount > 0 {
		parts = append(parts, fmt.Sprintf("count=%d", *r.KeepCount))
	}
	if r.KeepAgeDays != nil && *r.KeepAgeDays > 0 {
		parts = append(parts, fmt.Sprintf("age=%dd", *r.KeepAgeDays))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// renderEncrypted emits "yes" / "no" per D-20.
func renderEncrypted(r apiclient.BackupRepo) string {
	if r.EncryptionEnabled {
		return "yes"
	}
	return "no"
}

func runList(cmd *cobra.Command, args []string) error {
	storeName := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	repos, err := client.ListBackupRepos(storeName)
	if err != nil {
		return fmt.Errorf("failed to list backup repos: %w", err)
	}

	emptyHint := fmt.Sprintf(
		"No repos attached. Run: dfsctl store metadata %s repo add --name <name> --kind <local|s3>",
		storeName,
	)
	return cmdutil.PrintOutput(os.Stdout, repos, len(repos) == 0, emptyHint, RepoList(repos))
}
