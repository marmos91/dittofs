// Package payload implements payload store management commands.
package payload

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for payload store management.
var Cmd = &cobra.Command{
	Use:   "payload",
	Short: "Manage payload stores",
	Long: `Manage payload stores on the DittoFS server.

Payload stores hold actual file content data.
Supported types: memory, filesystem, s3

Examples:
  # List payload stores
  dfsctl store payload list

  # Add a memory store
  dfsctl store payload add --name fast-content --type memory

  # Add an S3 store
  dfsctl store payload add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(removeCmd)
}
