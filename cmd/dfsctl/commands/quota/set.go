package quota

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	setScope        string
	setID           int64
	setLimitBytes   string
	setSoftBytes    string
	setLimitFiles   int64
	setSoftFiles    int64
	setGraceSeconds int64
)

var setCmd = &cobra.Command{
	Use:   "set <share>",
	Short: "Create or update a per-identity quota",
	Long: `Create or update a per-identity quota on a share.

The --scope flag selects user, group, or default-user. For user/group scopes an
identity --id (uid or gid) is required. The default-user scope is a fallback
applied to any user without an explicit user quota and takes no --id.

A byte or file limit of 0 (the default) means "no limit on that dimension".

Examples:
  # Per-user quota (uid 1000): 10GiB / 100k files
  dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

  # Default-user fallback quota
  dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

  # Per-group quota with soft thresholds and a 7-day grace period
  dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800`,
	Args: cobra.ExactArgs(1),
	RunE: runSet,
}

func init() {
	setCmd.Flags().StringVar(&setScope, "scope", "", "Quota scope (user|group|default-user) (required)")
	setCmd.Flags().Int64Var(&setID, "id", -1, "Identity id (uid for user, gid for group). Required for user/group; omit for default-user.")
	setCmd.Flags().StringVar(&setLimitBytes, "limit-bytes", "", "Hard byte ceiling (e.g., '10GiB', '500MiB'). 0/empty = unlimited.")
	setCmd.Flags().StringVar(&setSoftBytes, "soft-bytes", "", "Soft byte threshold (e.g., '8GiB'). 0/empty = none.")
	setCmd.Flags().Int64Var(&setLimitFiles, "limit-files", 0, "Hard inode (file-count) ceiling. 0 = unlimited.")
	setCmd.Flags().Int64Var(&setSoftFiles, "soft-files", 0, "Soft inode threshold. 0 = none.")
	setCmd.Flags().Int64Var(&setGraceSeconds, "grace-seconds", 0, "Seconds usage may exceed a soft threshold before it is enforced as hard. 0 = grace disabled.")
	_ = setCmd.MarkFlagRequired("scope")
}

func runSet(cmd *cobra.Command, args []string) error {
	share := args[0]

	if !isValidScope(setScope) {
		return fmt.Errorf("invalid --scope %q (want user|group|default-user)", setScope)
	}

	id, err := resolveID(setScope, setID)
	if err != nil {
		return err
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	req := &apiclient.UpsertQuotaRequest{
		LimitBytes:   setLimitBytes,
		SoftBytes:    setSoftBytes,
		LimitFiles:   setLimitFiles,
		SoftFiles:    setSoftFiles,
		GraceSeconds: setGraceSeconds,
	}

	q, err := client.SetQuota(share, setScope, id, req)
	if err != nil {
		return fmt.Errorf("failed to set quota: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, q,
		fmt.Sprintf("Quota for scope '%s' on share '%s' set successfully", setScope, share))
}
