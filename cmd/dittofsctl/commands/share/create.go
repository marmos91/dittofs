package share

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	createName        string
	createMetadata    string
	createContent     string
	createReadOnly    bool
	createDescription string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new share",
	Long: `Create a new share on the DittoFS server.

Examples:
  # Create a share with required stores
  dittofsctl share create --name /archive --metadata default --content s3-store

  # Create a read-only share
  dittofsctl share create --name /readonly --metadata default --content fs-store --read-only

  # Create with description
  dittofsctl share create --name /docs --metadata default --content s3-store --description "Documentation files"`,
	RunE: runCreate,
}

func init() {
	createCmd.Flags().StringVar(&createName, "name", "", "Share name/path (required)")
	createCmd.Flags().StringVar(&createMetadata, "metadata", "", "Metadata store name (required)")
	createCmd.Flags().StringVar(&createContent, "content", "", "Content store name (required)")
	createCmd.Flags().BoolVar(&createReadOnly, "read-only", false, "Make share read-only")
	createCmd.Flags().StringVar(&createDescription, "description", "", "Share description")
	_ = createCmd.MarkFlagRequired("name")
	_ = createCmd.MarkFlagRequired("metadata")
	_ = createCmd.MarkFlagRequired("content")
}

func runCreate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	req := &apiclient.CreateShareRequest{
		Name:          createName,
		MetadataStore: createMetadata,
		ContentStore:  createContent,
		ReadOnly:      createReadOnly,
		Description:   createDescription,
	}

	share, err := client.CreateShare(req)
	if err != nil {
		return fmt.Errorf("failed to create share: %w", err)
	}

	return cmdutil.PrintCreatedResource(os.Stdout, share, fmt.Sprintf("Share '%s' created successfully", share.Name))
}
