package quota

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	removeScope string
	removeID    int64
	removeForce bool
)

var removeCmd = &cobra.Command{
	Use:   "remove <share>",
	Short: "Remove a per-identity quota",
	Long: `Remove a per-identity quota from a share.

Once removed, the identity reverts to the default-user fallback quota (if
one exists) or becomes unlimited. The operation is irreversible and requires
confirmation unless --force is specified.

Examples:
  # Remove a per-user quota (uid 1000)
  dfsctl quota remove /archive --scope user --id 1000

  # Remove the default-user fallback quota
  dfsctl quota remove /archive --scope default-user

  # Remove a per-group quota (gid 2000) without prompting
  dfsctl quota remove /archive --scope group --id 2000 --force`,
	Args: cobra.ExactArgs(1),
	RunE: runRemove,
}

func init() {
	removeCmd.Flags().StringVar(&removeScope, "scope", "", "Quota scope (user|group|default-user) (required)")
	removeCmd.Flags().Int64Var(&removeID, "id", -1, "Identity id (uid for user, gid for group). Required for user/group; omit for default-user.")
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
	_ = removeCmd.MarkFlagRequired("scope")
}

func runRemove(cmd *cobra.Command, args []string) error {
	share := args[0]

	if !isValidScope(removeScope) {
		return fmt.Errorf("invalid --scope %q (want user|group|default-user)", removeScope)
	}

	id, err := resolveID(removeScope, removeID)
	if err != nil {
		return err
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	label := fmt.Sprintf("%s/%s", share, removeScope)
	if id != nil {
		label = fmt.Sprintf("%s/%d", label, *id)
	}

	return cmdutil.RunDeleteWithConfirmation("Quota", label, removeForce, func() error {
		if err := client.RemoveQuota(share, removeScope, id); err != nil {
			return fmt.Errorf("failed to remove quota: %w", err)
		}
		return nil
	})
}
