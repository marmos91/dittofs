// Package block implements block store management commands.
package block

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/block/local"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/block/remote"
	"github.com/spf13/cobra"
)

// Cmd is the parent command for block store management.
var Cmd = &cobra.Command{
	Use:   "block",
	Short: "Block store management",
	Long: `Manage local and remote block stores on the DittoFS server.

Block stores hold file content data as blocks. Local block stores provide
fast disk-backed storage, while remote block stores provide durable cloud
storage (e.g., S3).

Examples:
  # List local block stores
  dfsctl store block local list

  # Add a local filesystem block store
  dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

  # List remote block stores
  dfsctl store block remote list

  # Add an S3 remote block store
  dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'`,
}

func init() {
	Cmd.AddCommand(local.Cmd)
	Cmd.AddCommand(remote.Cmd)
}
