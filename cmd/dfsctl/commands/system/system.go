// Package system implements system-level management commands.
package system

import "github.com/spf13/cobra"

// Cmd is the root command for system operations.
var Cmd = &cobra.Command{
	Use:   "system",
	Short: "System operations",
	Long: `System-level operations for managing the DittoFS server.

These commands expose low-level server controls that are not tied to a specific share or protocol. Currently available: drain-uploads, which blocks until all queued block-store uploads have completed.

Note: garbage collection (reclaiming space from deleted files) is NOT here — it
runs automatically in the background and is also available on demand via
"dfsctl store block gc <share>" (see the Garbage Collection guide).

Examples:
  # Wait for all in-flight uploads to finish (useful before benchmarking)
  dfsctl system drain-uploads`,
}

func init() {
	Cmd.AddCommand(drainUploadsCmd)
}
