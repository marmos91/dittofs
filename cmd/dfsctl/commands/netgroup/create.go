package netgroup

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var createName string

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new netgroup",
	Long: `Create a new netgroup on the DittoFS server.

Examples:
  # Create a netgroup
  dfsctl netgroup create --name office-network

  # Create and output as JSON
  dfsctl netgroup create --name office-network -o json`,
	RunE: runCreate,
}

func init() {
	createCmd.Flags().StringVar(&createName, "name", "", "Netgroup name (required)")
	_ = createCmd.MarkFlagRequired("name")
}

func runCreate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	netgroup, err := client.CreateNetgroup(createName)
	if err != nil {
		return fmt.Errorf("failed to create netgroup: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, netgroup, fmt.Sprintf("Netgroup '%s' created successfully", netgroup.Name))
}
