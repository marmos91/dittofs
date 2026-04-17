package repo

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <store-name> <repo-name>",
	Short: "Show backup repo details",
	Long: `Show detailed information about a backup repo, including schedule,
retention, encryption, and destination config.

S3 credentials (access_key_id, secret_access_key) are masked as *** in the
default table output. The raw values are passed through in -o json / -o yaml
(server-side redaction policy lives in the REST handler, see D-15).

Examples:
  # Show as table (secrets masked)
  dfsctl store metadata fast-meta repo show daily-s3

  # Full JSON shape
  dfsctl store metadata fast-meta repo show daily-s3 -o json`,
	Args: cobra.ExactArgs(2),
	RunE: runShow,
}

// RepoDetail wraps a BackupRepo for grouped-section table rendering.
type RepoDetail struct {
	r *apiclient.BackupRepo
}

// Headers implements TableRenderer.
func (d RepoDetail) Headers() []string { return []string{"FIELD", "VALUE"} }

// Rows implements TableRenderer. Groups core fields, retention, encryption,
// timestamps, and destination-specific config.
func (d RepoDetail) Rows() [][]string {
	rows := [][]string{
		{"Name", d.r.Name},
		{"Kind", d.r.Kind},
		{"Target", fmt.Sprintf("%s/%s", d.r.TargetKind, d.r.TargetID)},
	}
	if d.r.Schedule != nil && *d.r.Schedule != "" {
		rows = append(rows, []string{"Schedule", *d.r.Schedule})
	} else {
		rows = append(rows, []string{"Schedule", "-"})
	}
	rows = append(rows, []string{"Retention", renderRetention(*d.r)})
	rows = append(rows, []string{"Encrypted", renderEncrypted(*d.r)})
	if d.r.EncryptionKeyRef != "" {
		rows = append(rows, []string{"KeyRef", d.r.EncryptionKeyRef})
	}
	rows = append(rows, []string{"Created", d.r.CreatedAt.Format("2006-01-02 15:04:05")})
	rows = append(rows, []string{"Updated", d.r.UpdatedAt.Format("2006-01-02 15:04:05")})
	rows = append(rows, renderConfigRows(d.r)...)
	return rows
}

// renderConfigRows summarises the destination config in table mode. For S3
// credentials we mask access_key_id + secret_access_key as *** — defense in
// depth on top of whatever the server chooses to redact (T-06-04-01).
// Callers using -o json / -o yaml get the raw map and can decide their own
// handling (shells, pipelines, dittofs-pro UI).
func renderConfigRows(r *apiclient.BackupRepo) [][]string {
	rows := [][]string{}
	switch r.Kind {
	case "local":
		if v, ok := r.Config["path"]; ok {
			rows = append(rows, []string{"Path", fmt.Sprint(v)})
		}
	case "s3":
		for _, k := range []string{"bucket", "region", "endpoint", "prefix"} {
			if v, ok := r.Config[k]; ok && fmt.Sprint(v) != "" {
				rows = append(rows, []string{titleize(k), fmt.Sprint(v)})
			}
		}
		// Mask credentials in table mode regardless of server response.
		// secret_access_key is always masked; access_key_id masked too because
		// the ID is still PII that belongs in a secrets vault, not a terminal
		// log.
		if _, ok := r.Config["access_key_id"]; ok {
			rows = append(rows, []string{"AccessKeyID", "***"})
		}
		if _, ok := r.Config["secret_access_key"]; ok {
			rows = append(rows, []string{"SecretAccessKey", "***"})
		}
	}
	return rows
}

// titleize upper-cases the first letter of the keyword (keeps the rest as-is).
// We avoid strings.Title which is deprecated and locale-aware.
func titleize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func runShow(cmd *cobra.Command, args []string) error {
	storeName, repoName := args[0], args[1]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	r, err := client.GetBackupRepo(storeName, repoName)
	if err != nil {
		return fmt.Errorf("failed to get backup repo: %w", err)
	}

	format, fmtErr := cmdutil.GetOutputFormatParsed()
	if fmtErr != nil {
		return fmtErr
	}
	if format != output.FormatTable {
		// -o json / -o yaml: full BackupRepo shape (config map passed
		// through verbatim — CLI-side masking is table-mode only, see
		// renderConfigRows comment + T-06-04-01).
		return cmdutil.PrintResource(os.Stdout, r, nil)
	}
	return output.PrintTable(os.Stdout, RepoDetail{r: r})
}
