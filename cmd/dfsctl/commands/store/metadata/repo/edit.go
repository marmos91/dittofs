package repo

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	editSchedule         string
	editKeepCount        int
	editKeepAgeDays      int
	editEncryption       string // "on" | "off"
	editEncryptionKeyRef string
)

var editCmd = &cobra.Command{
	Use:   "edit <store-name> <repo-name>",
	Short: "Edit a backup repo (partial patch)",
	Long: `Edit a backup repo. Only flags you pass are updated; every other
field preserves its current DB value (D-19 partial patch).

Cannot change Kind or destination Config via this verb — reissue the repo
via 'remove' + 're-add' if a destination migration is needed.

Toggling encryption post-creation emits a server-side WARN: past archives
keep their original encryption status; restore honors the per-manifest
encryption flag (D-22).

Examples:
  # Adjust retention
  dfsctl store metadata fast-meta repo edit daily-s3 --keep-count 14

  # Drop schedule (becomes on-demand only)
  dfsctl store metadata fast-meta repo edit daily-s3 --schedule ""

  # Turn encryption on with a new key-ref
  dfsctl store metadata fast-meta repo edit daily-s3 \
    --encryption on --encryption-key-ref env:BACKUP_KEY`,
	Args: cobra.ExactArgs(2),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringVar(&editSchedule, "schedule", "", `Cron schedule (validated server-side; pass "" to clear)`)
	editCmd.Flags().IntVar(&editKeepCount, "keep-count", 0, "Retention count (0 = clear count policy)")
	editCmd.Flags().IntVar(&editKeepAgeDays, "keep-age-days", 0, "Retention age in days (0 = clear age policy)")
	editCmd.Flags().StringVar(&editEncryption, "encryption", "", "Encryption: on or off")
	editCmd.Flags().StringVar(&editEncryptionKeyRef, "encryption-key-ref", "", "env:VAR or file:/path")

	Cmd.AddCommand(editCmd)
}

func runEdit(cmd *cobra.Command, args []string) error {
	storeName, repoName := args[0], args[1]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	req, err := buildEditRequest(cmd)
	if err != nil {
		return err
	}

	r, err := client.UpdateBackupRepo(storeName, repoName, req)
	if err != nil {
		return fmt.Errorf("failed to update backup repo: %w", err)
	}
	return cmdutil.PrintResourceWithSuccess(
		os.Stdout,
		r,
		fmt.Sprintf("Backup repo '%s' on store '%s' updated.", r.Name, storeName),
	)
}

// buildEditRequest applies D-19 partial-patch semantics: only flags the
// operator actually passed make it into the request. cobra's
// Flags().Changed() is the authoritative presence check.
func buildEditRequest(cmd *cobra.Command) (*apiclient.BackupRepoRequest, error) {
	req := &apiclient.BackupRepoRequest{}
	changed := 0

	if cmd.Flags().Changed("schedule") {
		s := editSchedule
		req.Schedule = &s
		changed++
	}
	if cmd.Flags().Changed("keep-count") {
		v := editKeepCount
		req.KeepCount = &v
		changed++
	}
	if cmd.Flags().Changed("keep-age-days") {
		v := editKeepAgeDays
		req.KeepAgeDays = &v
		changed++
	}
	if cmd.Flags().Changed("encryption") {
		on := editEncryption == "on"
		req.EncryptionEnabled = &on
		changed++
	}
	if cmd.Flags().Changed("encryption-key-ref") {
		v := editEncryptionKeyRef
		req.EncryptionKeyRef = &v
		changed++
	}

	if changed == 0 {
		return nil, fmt.Errorf("No fields to update. Pass at least one of --schedule / --keep-count / --keep-age-days / --encryption / --encryption-key-ref.")
	}

	return req, nil
}
