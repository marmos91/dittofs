// Package store implements store management commands for dfsctl.
package store

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/block"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata"
	"github.com/spf13/cobra"
)

// Cmd is the parent command for store management.
var Cmd = &cobra.Command{
	Use:   "store",
	Short: "Store management",
	Long: `Manage metadata and block stores on the DittoFS server.

Store commands allow you to create, list, update, and delete stores.
These operations require admin privileges.

Examples:
  # List metadata stores
  dfsctl store metadata list

  # Add a new metadata store
  dfsctl store metadata add --name new-meta --type memory

  # List local block stores
  dfsctl store block local list

  # List remote block stores
  dfsctl store block remote list

  # Add a local block store
  dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

  # Add a remote block store
  dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket"}'`,
}

func init() {
	Cmd.AddCommand(metadata.Cmd)
	Cmd.AddCommand(block.Cmd)
}
