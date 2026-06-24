package group

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	editGID         uint32
	editDescription string
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a group",
	Long: `Edit an existing group on the DittoFS server. When run without flags, an
interactive prompt walks you through each editable field (GID and description),
showing the current value so you can press Enter to keep it. When flags are
provided, only those fields are updated and no prompt appears.

Examples:
  # Edit the group interactively (shows current values)
  dfsctl group edit editors

  # Change the group's GID to a specific value
  dfsctl group edit editors --gid 1002

  # Update only the description
  dfsctl group edit editors --description "Senior content editors"

  # Update both GID and description in one command
  dfsctl group edit editors --gid 1002 --description "Senior content editors"`,
	Args: cobra.ExactArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().Uint32Var(&editGID, "gid", 0, "Group ID")
	editCmd.Flags().StringVar(&editDescription, "description", "", "Group description")
}

func runEdit(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Check if any flags were provided
	hasFlags := cmd.Flags().Changed("gid") || cmd.Flags().Changed("description")

	// If no flags provided, run interactive mode
	if !hasFlags {
		return runEditInteractive(client, name)
	}

	req := &apiclient.UpdateGroupRequest{}
	hasUpdate := false

	if cmd.Flags().Changed("gid") {
		req.GID = &editGID
		hasUpdate = true
	}

	if editDescription != "" {
		req.Description = &editDescription
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no fields specified. Use --gid or --description")
	}

	group, err := client.UpdateGroup(name, req)
	if err != nil {
		return fmt.Errorf("failed to update group: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, group, fmt.Sprintf("Group '%s' updated successfully", group.Name))
}

func runEditInteractive(client *apiclient.Client, name string) error {
	// Fetch current group
	current, err := client.GetGroup(name)
	if err != nil {
		return fmt.Errorf("failed to get group: %w", err)
	}

	fmt.Printf("Editing group: %s\n", current.Name)
	fmt.Println("Press Enter to keep current value, or enter a new value.")
	fmt.Println("Press Ctrl+C to abort.")
	fmt.Println()

	req := &apiclient.UpdateGroupRequest{}
	hasUpdate := false

	// GID
	currentGIDStr := ""
	if current.GID != nil {
		currentGIDStr = fmt.Sprintf("%d", *current.GID)
	}
	newGIDStr, err := prompt.Input("GID", currentGIDStr)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newGIDStr != currentGIDStr {
		var newGID uint32
		if _, err := fmt.Sscanf(newGIDStr, "%d", &newGID); err == nil {
			req.GID = &newGID
			hasUpdate = true
		}
	}

	// Description
	newDescription, err := prompt.Input("Description", current.Description)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newDescription != current.Description {
		req.Description = &newDescription
		hasUpdate = true
	}

	if !hasUpdate {
		fmt.Println("No changes made.")
		return nil
	}

	group, err := client.UpdateGroup(name, req)
	if err != nil {
		return fmt.Errorf("failed to update group: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, group, fmt.Sprintf("Group '%s' updated successfully", group.Name))
}
