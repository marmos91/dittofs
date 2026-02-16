package user

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	editEmail       string
	editDisplayName string
	editRole        string
	editUID         uint32
	editGroups      string
	editEnabled     string // "true", "false", or "" for unchanged
)

var editCmd = &cobra.Command{
	Use:   "edit <username>",
	Short: "Edit a user",
	Long: `Edit an existing user on the DittoFS server.

When run without flags, opens an interactive editor to modify user properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit user interactively
  dfsctl user edit alice

  # Update email directly
  dfsctl user edit alice --email alice@newdomain.com

  # Update role to admin
  dfsctl user edit alice --role admin

  # Disable user
  dfsctl user edit alice --enabled false

  # Update multiple fields
  dfsctl user edit alice --email alice@example.com --groups editors,admins

  # Update UID
  dfsctl user edit alice --uid 1001`,
	Args: cobra.ExactArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringVar(&editEmail, "email", "", "Email address")
	editCmd.Flags().StringVar(&editDisplayName, "display-name", "", "Display name")
	editCmd.Flags().StringVar(&editRole, "role", "", "Role (user|admin)")
	editCmd.Flags().Uint32Var(&editUID, "uid", 0, "Unix user ID")
	editCmd.Flags().StringVar(&editGroups, "groups", "", "Comma-separated list of groups")
	editCmd.Flags().StringVar(&editEnabled, "enabled", "", "Enable/disable account (true|false)")
}

func runEdit(cmd *cobra.Command, args []string) error {
	username := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Check if any flags were provided
	hasFlags := cmd.Flags().Changed("email") || cmd.Flags().Changed("display-name") ||
		cmd.Flags().Changed("role") || cmd.Flags().Changed("uid") ||
		cmd.Flags().Changed("groups") || cmd.Flags().Changed("enabled")

	// If no flags provided, run interactive mode
	if !hasFlags {
		return runEditInteractive(client, username)
	}

	req := &apiclient.UpdateUserRequest{}
	hasUpdate := false

	if editEmail != "" {
		req.Email = &editEmail
		hasUpdate = true
	}

	if editDisplayName != "" {
		req.DisplayName = &editDisplayName
		hasUpdate = true
	}

	if editRole != "" {
		req.Role = &editRole
		hasUpdate = true
	}

	if cmd.Flags().Changed("uid") {
		req.UID = &editUID
		hasUpdate = true
	}

	if editGroups != "" {
		groups := cmdutil.ParseCommaSeparatedList(editGroups)
		req.Groups = &groups
		hasUpdate = true
	}

	if editEnabled != "" {
		enabled := strings.ToLower(editEnabled) == "true"
		req.Enabled = &enabled
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no fields specified. Use --email, --role, --uid, --groups, or --enabled")
	}

	user, err := client.UpdateUser(username, req)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, user, fmt.Sprintf("User '%s' updated successfully", user.Username))
}

func runEditInteractive(client *apiclient.Client, username string) error {
	// Fetch current user
	current, err := client.GetUser(username)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	fmt.Printf("Editing user: %s\n", current.Username)
	fmt.Println("Press Enter to keep current value, or enter a new value.")
	fmt.Println("Press Ctrl+C to abort.")
	fmt.Println()

	req := &apiclient.UpdateUserRequest{}
	hasUpdate := false

	// Email
	newEmail, err := prompt.Input("Email", current.Email)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newEmail != current.Email {
		req.Email = &newEmail
		hasUpdate = true
	}

	// Role
	roleOptions := []prompt.SelectOption{
		{Label: "user", Value: "user", Description: "Regular user with standard permissions"},
		{Label: "admin", Value: "admin", Description: "Administrator with full access"},
	}
	fmt.Printf("Current role: %s\n", current.Role)
	newRole, err := prompt.Select("Role", roleOptions)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newRole != current.Role {
		req.Role = &newRole
		hasUpdate = true
	}

	// UID
	currentUIDStr := ""
	if current.UID != nil {
		currentUIDStr = fmt.Sprintf("%d", *current.UID)
	}
	newUIDStr, err := prompt.Input("UID (leave empty to keep current)", currentUIDStr)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newUIDStr != currentUIDStr && newUIDStr != "" {
		var newUID uint32
		if _, err := fmt.Sscanf(newUIDStr, "%d", &newUID); err == nil {
			req.UID = &newUID
			hasUpdate = true
		}
	}

	// Groups
	currentGroups := strings.Join(current.Groups, ",")
	newGroups, err := prompt.Input("Groups (comma-separated)", currentGroups)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newGroups != currentGroups {
		groups := cmdutil.ParseCommaSeparatedList(newGroups)
		req.Groups = &groups
		hasUpdate = true
	}

	// Enabled
	enabledOptions := []prompt.SelectOption{
		{Label: "enabled", Value: "true", Description: "User can log in"},
		{Label: "disabled", Value: "false", Description: "User cannot log in"},
	}
	currentStatus := "enabled"
	if !current.Enabled {
		currentStatus = "disabled"
	}
	fmt.Printf("Currently: %s\n", currentStatus)
	newEnabledStr, err := prompt.Select("Account status", enabledOptions)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	newEnabled := newEnabledStr == "true"
	if newEnabled != current.Enabled {
		req.Enabled = &newEnabled
		hasUpdate = true
	}

	if !hasUpdate {
		fmt.Println("No changes made.")
		return nil
	}

	user, err := client.UpdateUser(username, req)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, user, fmt.Sprintf("User '%s' updated successfully", user.Username))
}
