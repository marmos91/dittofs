// Package store implements store management commands for dfsctl.
package store

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/payload"
	"github.com/spf13/cobra"
)

// Cmd is the parent command for store management.
var Cmd = &cobra.Command{
	Use:   "store",
	Short: "Store management",
	Long: `Manage metadata and payload stores on the DittoFS server.

Store commands allow you to create, list, update, and delete stores.
These operations require admin privileges.

Examples:
  # List metadata stores
  dfsctl store metadata list

  # Add a new metadata store
  dfsctl store metadata add --name new-meta --type memory

  # List payload stores
  dfsctl store payload list

  # Add a new payload store
  dfsctl store payload add --name s3-store --type s3 --config '{"bucket":"my-bucket"}'`,
}

func init() {
	Cmd.AddCommand(metadata.Cmd)
	Cmd.AddCommand(payload.Cmd)
}
