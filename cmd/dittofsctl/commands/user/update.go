package user

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	updateEmail       string
	updateDisplayName string
	updateRole        string
	updateGroups      string
	updateEnabled     string // "true", "false", or "" for unchanged
)

var updateCmd = &cobra.Command{
	Use:   "update <username>",
	Short: "Update a user",
	Long: `Update an existing user on the DittoFS server.

Only specified fields will be updated.

Examples:
  # Update email
  dittofsctl user update alice --email alice@newdomain.com

  # Update role to admin
  dittofsctl user update alice --role admin

  # Disable user
  dittofsctl user update alice --enabled false

  # Update multiple fields
  dittofsctl user update alice --email alice@example.com --groups editors,admins`,
	Args: cobra.ExactArgs(1),
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().StringVar(&updateEmail, "email", "", "Email address")
	updateCmd.Flags().StringVar(&updateDisplayName, "display-name", "", "Display name")
	updateCmd.Flags().StringVar(&updateRole, "role", "", "Role (user|admin)")
	updateCmd.Flags().StringVar(&updateGroups, "groups", "", "Comma-separated list of groups")
	updateCmd.Flags().StringVar(&updateEnabled, "enabled", "", "Enable/disable account (true|false)")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	username := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	req := &apiclient.UpdateUserRequest{}
	hasUpdate := false

	if updateEmail != "" {
		req.Email = &updateEmail
		hasUpdate = true
	}

	if updateDisplayName != "" {
		req.DisplayName = &updateDisplayName
		hasUpdate = true
	}

	if updateRole != "" {
		req.Role = &updateRole
		hasUpdate = true
	}

	if updateGroups != "" {
		groups := cmdutil.ParseCommaSeparatedList(updateGroups)
		req.Groups = &groups
		hasUpdate = true
	}

	if updateEnabled != "" {
		enabled := strings.ToLower(updateEnabled) == "true"
		req.Enabled = &enabled
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no update fields specified. Use --email, --role, --groups, or --enabled")
	}

	user, err := client.UpdateUser(username, req)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, user, fmt.Sprintf("User '%s' updated successfully", user.Username))
}
