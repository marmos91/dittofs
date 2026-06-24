// Package bench implements filesystem benchmark commands for dfsctl.
package bench

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for filesystem benchmarks.
var Cmd = &cobra.Command{
	Use:   "bench",
	Short: "Run filesystem benchmarks",
	Long: `Run I/O and metadata benchmarks against any mounted filesystem path.

The benchmark suite operates directly on the filesystem; no API authentication is needed for basic workloads. Use 'bench run' to collect results and save them as JSON, then 'bench compare' to render a side-by-side comparison across systems. The 'bench storage-tiers' subcommand requires admin authentication to evict cache layers between reads.

Examples:
  # Run all benchmark workloads on a mounted NFS share
  dfsctl bench run /mnt/bench

  # Run with 8 threads and 512 MiB files
  dfsctl bench run /mnt/bench --threads 8 --file-size 512MiB --duration 30s

  # Run and save results for later comparison
  dfsctl bench run /mnt/bench --system dittofs --save results/dittofs.json

  # Compare saved results from two systems
  dfsctl bench compare results/dittofs.json results/kernel-nfs.json`,
}

func init() {
	Cmd.AddCommand(runCmd)
	Cmd.AddCommand(compareCmd)
	Cmd.AddCommand(storageTiersCmd)
}
