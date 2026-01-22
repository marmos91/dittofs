package share

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	updateReadOnly    string
	updateDescription string
)

var updateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update a share",
	Long: `Update an existing share on the DittoFS server.

Only specified fields will be updated.

Examples:
  # Make share read-only
  dittofsctl share update /archive --read-only true

  # Make share writable
  dittofsctl share update /archive --read-only false

  # Update description
  dittofsctl share update /archive --description "New description"`,
	Args: cobra.ExactArgs(1),
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().StringVar(&updateReadOnly, "read-only", "", "Set read-only (true|false)")
	updateCmd.Flags().StringVar(&updateDescription, "description", "", "Share description")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Build update request with only specified fields
	req := &apiclient.UpdateShareRequest{}
	hasUpdate := false

	if updateReadOnly != "" {
		readOnly := strings.ToLower(updateReadOnly) == "true"
		req.ReadOnly = &readOnly
		hasUpdate = true
	}

	if updateDescription != "" {
		req.Description = &updateDescription
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no update fields specified. Use --read-only or --description")
	}

	share, err := client.UpdateShare(name, req)
	if err != nil {
		return fmt.Errorf("failed to update share: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share '%s' updated successfully", share.Name))
}
