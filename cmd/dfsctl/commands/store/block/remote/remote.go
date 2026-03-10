// Package remote implements remote block store management commands.
package remote

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for remote block store management.
var Cmd = &cobra.Command{
	Use:   "remote",
	Short: "Remote block store management",
	Long: `Manage remote block stores on the DittoFS server.

Remote block stores provide durable cloud storage for file content blocks.
Supported types: s3 (AWS S3 or S3-compatible), memory (testing)

Examples:
  # List remote block stores
  dfsctl store block remote list

  # Add an S3 block store
  dfsctl store block remote add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'

  # Add a memory block store (for testing)
  dfsctl store block remote add --name test-remote --type memory`,
}

func init() {
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(removeCmd)
}
