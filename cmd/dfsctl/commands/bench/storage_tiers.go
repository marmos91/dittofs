package bench

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/bench"
	"github.com/spf13/cobra"
)

var (
	storageTiersShare string
	storageTiersMount string
	storageTiersSizes string
)

var storageTiersCmd = &cobra.Command{
	Use:   "storage-tiers",
	Short: "Benchmark storage tier performance (cold/warm/local-only)",
	Long: `Benchmark DittoFS storage tier performance by measuring read throughput at each caching layer.

The workload writes a file through the NFS/SMB mount, then reads it back three times — evicting a different cache layer before each read — to isolate cold (remote store), warm (local + read buffer), and local-only performance. Admin authentication is required to call the eviction API between reads. The share must have a remote block store configured for cold-read testing.

Steps executed per file size:
  1. Write via mount
  2. Evict all (read buffer + local store)
  3. Cold read (data fetched from remote store)
  4. Warm read (data in read buffer + local store)
  5. Evict read buffer only
  6. Local-only read (data served from local FS store)

Examples:
  # Run with default file sizes (10MB, 100MB, 1GB)
  dfsctl bench storage-tiers --share myshare --mount /mnt/test

  # Run with custom file sizes
  dfsctl bench storage-tiers --share myshare --mount /mnt/test --sizes 1MB,10MB,50MB`,
	RunE: runStorageTiers,
}

func init() {
	storageTiersCmd.Flags().StringVar(&storageTiersShare, "share", "", "Share name for block store API operations (required)")
	storageTiersCmd.Flags().StringVar(&storageTiersMount, "mount", "/mnt/test", "Mount point for file I/O")
	storageTiersCmd.Flags().StringVar(&storageTiersSizes, "sizes", "", "Comma-separated file sizes (default: 10MB,100MB,1GB)")

	_ = storageTiersCmd.MarkFlagRequired("share")
}

func runStorageTiers(cmd *cobra.Command, _ []string) error {
	// Get authenticated client for block store operations.
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return fmt.Errorf("authentication required for storage-tiers benchmark: %w", err)
	}

	// Parse file sizes.
	var fileSizes []int64
	if storageTiersSizes != "" {
		for _, s := range cmdutil.ParseCommaSeparatedList(storageTiersSizes) {
			size, err := bench.ParseSize(s)
			if err != nil {
				return fmt.Errorf("invalid size %q: %w", s, err)
			}
			fileSizes = append(fileSizes, size)
		}
	}

	// Validate mount point.
	info, err := os.Stat(storageTiersMount)
	if err != nil {
		return fmt.Errorf("mount point %q: %w", storageTiersMount, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mount point %q is not a directory", storageTiersMount)
	}

	cfg := bench.StorageTiersConfig{
		MountPoint: storageTiersMount,
		ShareName:  storageTiersShare,
		Client:     client,
		FileSizes:  fileSizes,
	}

	bm := bench.NewStorageTiersBenchmark(cfg)

	fmt.Fprintf(os.Stderr, "Storage Tiers Benchmark - Share: %s\n", storageTiersShare)
	fmt.Fprintf(os.Stderr, "Mount: %s\n", storageTiersMount)

	// NewStorageTiersBenchmark already applies defaults, so use its resolved sizes.
	sizeLabels := make([]string, len(bm.Sizes()))
	for i, s := range bm.Sizes() {
		sizeLabels[i] = bench.FormatSize(s)
	}
	fmt.Fprintf(os.Stderr, "Sizes: %v\n\n", sizeLabels)

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format, args...)
	}

	result, err := bm.Run(cmd.Context(), logf)
	if err != nil {
		return fmt.Errorf("benchmark failed: %w", err)
	}

	if len(result.Results) == 0 {
		fmt.Fprintln(os.Stderr, "\nNo results collected (all sizes failed)")
		return nil
	}

	// Print results table.
	fmt.Fprintln(os.Stderr)
	return cmdutil.PrintResource(os.Stdout, result, StorageTiersTable{Result: result})
}

// StorageTiersTable renders storage-tiers benchmark results as a table.
type StorageTiersTable struct {
	Result *bench.StorageTiersResult
}

// Headers implements output.TableRenderer.
func (t StorageTiersTable) Headers() []string {
	return []string{"FILE SIZE", "STEP", "THROUGHPUT", "DURATION", "READ BUF HIT RATE"}
}

// Rows implements output.TableRenderer.
func (t StorageTiersTable) Rows() [][]string {
	var rows [][]string

	for _, r := range t.Result.Results {
		sizeLabel := bench.FormatSize(r.FileSize)

		rows = append(rows,
			formatStorageTiersRow(sizeLabel, "Write", r.WriteStats),
			formatStorageTiersRow(sizeLabel, "Cold Read", r.ColdReadStats),
			formatStorageTiersRow(sizeLabel, "Warm Read", r.WarmReadStats),
			formatStorageTiersRow(sizeLabel, "Local-Only Read", r.LocalOnlyStats),
		)
	}

	return rows
}

func formatStorageTiersRow(size, step string, stats bench.IOStats) []string {
	throughput := fmt.Sprintf("%.1f MB/s", stats.ThroughputMBps)

	duration := fmt.Sprintf("%dms", stats.DurationMs)
	if stats.DurationMs >= 1000 {
		duration = fmt.Sprintf("%.1fs", float64(stats.DurationMs)/1000)
	}

	hitRate := fmt.Sprintf("%.0f%%", stats.L1HitRate)
	if stats.L1HitRate < 0 {
		hitRate = "-"
	}

	return []string{size, step, throughput, duration, hitRate}
}
