package quota

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	rmScope string
	rmID    int64
	rmForce bool
)

var rmCmd = &cobra.Command{
	Use:   "rm <share>",
	Short: "Remove a per-identity quota",
	Long: `Remove a per-identity quota from a share.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Remove a per-user quota (uid 1000)
  dfsctl quota rm /archive --scope user --id 1000

  # Remove the default-user fallback quota
  dfsctl quota rm /archive --scope default-user

  # Remove without confirmation
  dfsctl quota rm /archive --scope group --id 2000 --force`,
	Args: cobra.ExactArgs(1),
	RunE: runRm,
}

func init() {
	rmCmd.Flags().StringVar(&rmScope, "scope", "", "Quota scope (user|group|default-user) (required)")
	rmCmd.Flags().Int64Var(&rmID, "id", -1, "Identity id (uid for user, gid for group). Required for user/group; omit for default-user.")
	rmCmd.Flags().BoolVarP(&rmForce, "force", "f", false, "Skip confirmation prompt")
	_ = rmCmd.MarkFlagRequired("scope")
}

func runRm(cmd *cobra.Command, args []string) error {
	share := args[0]

	if !isValidScope(rmScope) {
		return fmt.Errorf("invalid --scope %q (want user|group|default-user)", rmScope)
	}

	id, err := resolveID(rmScope, rmID)
	if err != nil {
		return err
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	label := fmt.Sprintf("%s/%s", share, rmScope)
	if id != nil {
		label = fmt.Sprintf("%s/%d", label, *id)
	}

	return cmdutil.RunDeleteWithConfirmation("Quota", label, rmForce, func() error {
		if err := client.DeleteQuota(share, rmScope, id); err != nil {
			return fmt.Errorf("failed to delete quota: %w", err)
		}
		return nil
	})
}
