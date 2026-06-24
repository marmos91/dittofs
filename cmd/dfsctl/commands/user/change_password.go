package user

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/spf13/cobra"
)

var (
	currentPassword string
	newPassword     string
)

var changePasswordCmd = &cobra.Command{
	Use:   "change-password",
	Short: "Change your own password",
	Long: `Change your own password as the currently authenticated user. This is
distinct from the admin-only "dfsctl user password" command: it verifies your
current password before accepting the new one, and updates the stored session
tokens automatically. You will be prompted for both passwords unless flags are
provided (flags are less secure as passwords may appear in shell history).

Examples:
  # Change password interactively (recommended — passwords are not echoed)
  dfsctl user change-password

  # Change password non-interactively (use with caution)
  dfsctl user change-password --current oldpass --new newpass`,
	RunE: runChangePassword,
}

func init() {
	changePasswordCmd.Flags().StringVarP(&currentPassword, "current", "c", "", "Current password (prompts if not provided)")
	changePasswordCmd.Flags().StringVarP(&newPassword, "new", "n", "", "New password (prompts if not provided)")
}

func runChangePassword(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Get current password interactively if not provided
	current := currentPassword
	if current == "" {
		current, err = prompt.Password("Current password")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	// Get new password interactively if not provided
	newPwd := newPassword
	if newPwd == "" {
		newPwd, err = prompt.PasswordWithConfirmation("New password", "Confirm new password", 8)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	// Change password and get new tokens
	tokens, err := client.ChangeOwnPassword(current, newPwd)
	if err != nil {
		return fmt.Errorf("failed to change password: %w", err)
	}

	// Update stored credentials with new tokens
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	if err := store.UpdateTokens(tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresAt); err != nil {
		return fmt.Errorf("failed to update stored credentials: %w", err)
	}

	cmdutil.PrintSuccess("Password changed successfully")

	return nil
}
