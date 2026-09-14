// Package block implements block store management commands.
package block

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for block store management.
var Cmd = &cobra.Command{
	Use:   "block",
	Short: "Block store management",
	Long: `Manage block stores on the DittoFS server.

Block stores hold file content data as blocks. Each share keeps a local
journal on disk, backed by the block store it is bound to.

Supported types: s3 (AWS S3 or S3-compatible), memory (testing)

Examples:
  # List block stores
  dfsctl store block list

  # Add an S3 block store
  dfsctl store block add --name s3-store --type s3 --bucket my-bucket --region us-east-1

  # Add a memory block store (for testing)
  dfsctl store block add --name test-store --type memory`,
}

func init() {
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(removeCmd)
	Cmd.AddCommand(statsCmd)
	Cmd.AddCommand(evictCmd)
	Cmd.AddCommand(healthCmd)
	Cmd.AddCommand(gcCmd)
	Cmd.AddCommand(gcStatusCmd)
	Cmd.AddCommand(auditRefcountsCmd)
	Cmd.AddCommand(reconcileCmd)
	Cmd.AddCommand(reclaimCmd)
}
