package share

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	createName              string
	createMetadata          string
	createPayload           string
	createReadOnly          bool
	createDefaultPermission string
	createDescription       string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new share",
	Long: `Create a new share on the DittoFS server.

Examples:
  # Create a share with required stores
  dittofsctl share create --name /archive --metadata default --payload s3-store

  # Create a read-only share
  dittofsctl share create --name /readonly --metadata default --payload fs-store --read-only

  # Create with default permission allowing all users read-write access
  dittofsctl share create --name /shared --metadata default --payload s3-store --default-permission read-write

  # Create with description
  dittofsctl share create --name /docs --metadata default --payload s3-store --description "Documentation files"`,
	RunE: runCreate,
}

func init() {
	createCmd.Flags().StringVar(&createName, "name", "", "Share name/path (required)")
	createCmd.Flags().StringVar(&createMetadata, "metadata", "", "Metadata store name (required)")
	createCmd.Flags().StringVar(&createPayload, "payload", "", "Payload store name (required)")
	createCmd.Flags().BoolVar(&createReadOnly, "read-only", false, "Make share read-only")
	createCmd.Flags().StringVar(&createDefaultPermission, "default-permission", "read-write", "Default permission (none|read|read-write|admin)")
	createCmd.Flags().StringVar(&createDescription, "description", "", "Share description")
}

func runCreate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	name := createName
	if name == "" {
		name, err = prompt.InputRequired("Share name (e.g., /export)")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	metadata := createMetadata
	if metadata == "" {
		metadata, err = prompt.InputRequired("Metadata store name")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	payload := createPayload
	if payload == "" {
		payload, err = prompt.InputRequired("Payload store name")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	defaultPerm := createDefaultPermission
	if !cmd.Flags().Changed("default-permission") && createName == "" {
		// Interactive mode - ask for default permission
		permOptions := []string{"read-write", "read", "admin", "none"}
		selectedPerm, err := prompt.SelectString("Default permission", permOptions)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
		defaultPerm = selectedPerm
	}

	req := &apiclient.CreateShareRequest{
		Name:              name,
		MetadataStoreID:   metadata,
		PayloadStoreID:    payload,
		ReadOnly:          createReadOnly,
		DefaultPermission: defaultPerm,
		Description:       createDescription,
	}

	share, err := client.CreateShare(req)
	if err != nil {
		return fmt.Errorf("failed to create share: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share '%s' created successfully", share.Name))
}
