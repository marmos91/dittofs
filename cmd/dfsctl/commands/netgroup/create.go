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
	Long: `Create a new netgroup on the DittoFS server. Netgroups are named sets of
IP addresses, CIDR ranges, or hostnames that can be referenced in share security
policies to control which network endpoints are allowed access. After creating a
netgroup, use "dfsctl netgroup add-member" to populate it.

Examples:
  # Create a netgroup for the office subnet
  dfsctl netgroup create --name office-network

  # Create a netgroup and output the result as JSON
  dfsctl netgroup create --name datacenter-hosts -o json`,
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
