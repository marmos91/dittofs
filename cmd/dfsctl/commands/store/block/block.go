// Package block implements block store management commands.
package block

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/block/remote"
	"github.com/spf13/cobra"
)

// Cmd is the parent command for block store management.
var Cmd = &cobra.Command{
	Use:   "block",
	Short: "Block store management",
	Long: `Manage remote block stores on the DittoFS server.

Block stores hold file content data as blocks. Each share keeps a local
journal on disk; a remote block store backs it with durable cloud storage
(e.g., S3).

Examples:
  # List remote block stores
  dfsctl store block remote list

  # Add an S3 remote block store
  dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'`,
}

func init() {
	Cmd.AddCommand(remote.Cmd)
	Cmd.AddCommand(statsCmd)
	Cmd.AddCommand(evictCmd)
	Cmd.AddCommand(healthCmd)
	Cmd.AddCommand(gcCmd)
	Cmd.AddCommand(gcStatusCmd)
	Cmd.AddCommand(auditRefcountsCmd)
	Cmd.AddCommand(reconcileCmd)
	Cmd.AddCommand(reclaimCmd)
}
