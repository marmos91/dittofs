package user

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/spf13/cobra"
)

var resetPassword string

var passwordCmd = &cobra.Command{
	Use:   "password <username>",
	Short: "Reset a user's password",
	Long: `Reset a user's password (admin operation).

This sets the user's password and marks them as needing to change it
on next login.

Examples:
  # Reset password interactively
  dittofsctl user password alice

  # Reset password with flag (less secure)
  dittofsctl user password alice --password newsecret`,
	Args: cobra.ExactArgs(1),
	RunE: runPassword,
}

func init() {
	passwordCmd.Flags().StringVarP(&resetPassword, "password", "p", "", "New password (prompts if not provided)")
}

func runPassword(cmd *cobra.Command, args []string) error {
	username := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Get password interactively if not provided
	password := resetPassword
	if password == "" {
		password, err = prompt.PasswordWithConfirmation("New password", "Confirm password", 8)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	if err := client.ResetUserPassword(username, password); err != nil {
		return fmt.Errorf("failed to reset password: %w", err)
	}

	cmdutil.PrintSuccessWithInfo(
		fmt.Sprintf("Password reset for user '%s'", username),
		"User will be required to change password on next login.",
	)

	return nil
}
