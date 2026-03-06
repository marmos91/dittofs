// Package bench implements filesystem benchmark commands for dfsctl.
package bench

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for filesystem benchmarks.
var Cmd = &cobra.Command{
	Use:   "bench",
	Short: "Run filesystem benchmarks",
	Long: `Run I/O and metadata benchmarks against any mounted filesystem.

Benchmarks operate directly on the filesystem — no API authentication required.
Use this to measure DittoFS performance or compare against other NFS/SMB servers.

Examples:
  # Run all benchmarks on a mounted NFS share
  dfsctl bench run /mnt/bench

  # Run with custom parameters
  dfsctl bench run /mnt/bench --threads 8 --file-size 512MiB --duration 30s

  # Run specific workloads and save results
  dfsctl bench run /mnt/bench --workload seq-write,seq-read --system dittofs --save results.json

  # Compare results from multiple systems
  dfsctl bench compare dittofs.json kernel-nfs.json ganesha.json`,
}

func init() {
	Cmd.AddCommand(runCmd)
	Cmd.AddCommand(compareCmd)
}
