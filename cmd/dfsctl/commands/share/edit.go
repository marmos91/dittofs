package share

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
	editReadOnly          string
	editDefaultPermission string
	editDescription       string
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a share",
	Long: `Edit an existing share on the DittoFS server.

When run without flags, opens an interactive editor to modify share properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit share interactively
  dfsctl share edit /archive

  # Make share read-only
  dfsctl share edit /archive --read-only true

  # Make share writable
  dfsctl share edit /archive --read-only false

  # Set default permission to allow all users read-write access
  dfsctl share edit /archive --default-permission read-write

  # Update description
  dfsctl share edit /archive --description "New description"`,
	Args: cobra.ExactArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringVar(&editReadOnly, "read-only", "", "Set read-only (true|false)")
	editCmd.Flags().StringVar(&editDefaultPermission, "default-permission", "", "Default permission (none|read|read-write|admin)")
	editCmd.Flags().StringVar(&editDescription, "description", "", "Share description")
}

func runEdit(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Check if any flags were provided
	hasFlags := cmd.Flags().Changed("read-only") || cmd.Flags().Changed("default-permission") ||
		cmd.Flags().Changed("description")

	// If no flags provided, run interactive mode
	if !hasFlags {
		return runEditInteractive(client, name)
	}

	// Build update request with only specified fields
	req := &apiclient.UpdateShareRequest{}
	hasUpdate := false

	if editReadOnly != "" {
		readOnly := strings.ToLower(editReadOnly) == "true"
		req.ReadOnly = &readOnly
		hasUpdate = true
	}

	if editDefaultPermission != "" {
		req.DefaultPermission = &editDefaultPermission
		hasUpdate = true
	}

	if editDescription != "" {
		req.Description = &editDescription
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no fields specified. Use --read-only, --default-permission, or --description")
	}

	share, err := client.UpdateShare(name, req)
	if err != nil {
		return fmt.Errorf("failed to update share: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share '%s' updated successfully", share.Name))
}

func runEditInteractive(client *apiclient.Client, name string) error {
	// Fetch current share
	current, err := client.GetShare(name)
	if err != nil {
		return fmt.Errorf("failed to get share: %w", err)
	}

	fmt.Printf("Editing share: %s\n", current.Name)
	fmt.Println("Press Enter to keep current value, or enter a new value.")
	fmt.Println("Press Ctrl+C to abort.")
	fmt.Println()

	req := &apiclient.UpdateShareRequest{}
	hasUpdate := false

	// Description
	newDescription, err := prompt.Input("Description", current.Description)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newDescription != current.Description {
		req.Description = &newDescription
		hasUpdate = true
	}

	// Read-only
	readOnlyOptions := []prompt.SelectOption{
		{Label: "writable", Value: "false", Description: "Allow write operations"},
		{Label: "read-only", Value: "true", Description: "Only allow read operations"},
	}
	currentReadOnlyStatus := "writable"
	if current.ReadOnly {
		currentReadOnlyStatus = "read-only"
	}
	fmt.Printf("Currently: %s\n", currentReadOnlyStatus)
	newReadOnlyStr, err := prompt.Select("Access mode", readOnlyOptions)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	newReadOnly := newReadOnlyStr == "true"
	if newReadOnly != current.ReadOnly {
		req.ReadOnly = &newReadOnly
		hasUpdate = true
	}

	// Default permission
	permOptions := []prompt.SelectOption{
		{Label: "none", Value: "none", Description: "No default access"},
		{Label: "read", Value: "read", Description: "Read-only access by default"},
		{Label: "read-write", Value: "read-write", Description: "Read-write access by default"},
		{Label: "admin", Value: "admin", Description: "Admin access by default"},
	}
	currentPerm := current.DefaultPermission
	if currentPerm == "" {
		currentPerm = "none"
	}
	fmt.Printf("Current default permission: %s\n", currentPerm)
	newPerm, err := prompt.Select("Default permission", permOptions)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newPerm != currentPerm {
		req.DefaultPermission = &newPerm
		hasUpdate = true
	}

	if !hasUpdate {
		fmt.Println("No changes made.")
		return nil
	}

	share, err := client.UpdateShare(name, req)
	if err != nil {
		return fmt.Errorf("failed to update share: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share '%s' updated successfully", share.Name))
}
