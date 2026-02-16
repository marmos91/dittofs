package group

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	createName        string
	createGID         uint32
	createDescription string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new group",
	Long: `Create a new group on the DittoFS server.

Examples:
  # Create a group
  dfsctl group create --name editors

  # Create a group with specific GID
  dfsctl group create --name editors --gid 1001

  # Create a group with description
  dfsctl group create --name editors --description "Content editors"`,
	RunE: runCreate,
}

func init() {
	createCmd.Flags().StringVar(&createName, "name", "", "Group name (required)")
	createCmd.Flags().Uint32Var(&createGID, "gid", 0, "Group ID (auto-generated if not set)")
	createCmd.Flags().StringVar(&createDescription, "description", "", "Group description")
	_ = createCmd.MarkFlagRequired("name")
}

func runCreate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	req := &apiclient.CreateGroupRequest{
		Name:        createName,
		Description: createDescription,
	}

	// Only set GID if explicitly provided
	if cmd.Flags().Changed("gid") {
		gid := createGID
		req.GID = &gid
	}

	group, err := client.CreateGroup(req)
	if err != nil {
		return fmt.Errorf("failed to create group: %w", err)
	}

	gidStr := "auto"
	if group.GID != nil {
		gidStr = fmt.Sprintf("%d", *group.GID)
	}
	return cmdutil.PrintResourceWithSuccess(os.Stdout, group, fmt.Sprintf("Group '%s' created successfully (GID: %s)", group.Name, gidStr))
}
