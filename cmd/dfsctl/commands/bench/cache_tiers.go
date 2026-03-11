package bench

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/bench"
	"github.com/spf13/cobra"
)

var (
	cacheTiersShare string
	cacheTiersMount string
	cacheTiersSizes string
)

var cacheTiersCmd = &cobra.Command{
	Use:   "cache-tiers",
	Short: "Benchmark cache tier performance (cold/warm/L2)",
	Long: `Run a 6-step cache tier benchmark that measures read performance at each
cache level by selectively evicting cache layers between reads.

This workload requires:
  - An authenticated session (cache eviction requires admin access)
  - A DittoFS server with a mounted share
  - The share must be configured with a remote store for cold read testing

The benchmark runs the following steps for each file size:
  1. Write: Create test file via NFS/SMB mount
  2. Evict all: Clear L1 + local cache via API
  3. Cold read: Read file (data from remote store)
  4. Warm read: Read file again (data in L1 + local cache)
  5. Evict L1: Clear memory cache only via API
  6. L2 read: Read file (data from local FS cache only)

Results show throughput and L1 cache hit rate per step.

Examples:
  # Run with default sizes (10MB, 100MB, 1GB)
  dfsctl bench cache-tiers --share /export --mount /mnt/test

  # Custom file sizes
  dfsctl bench cache-tiers --share /export --mount /mnt/test --sizes 1MB,10MB,50MB`,
	RunE: runCacheTiers,
}

func init() {
	cacheTiersCmd.Flags().StringVar(&cacheTiersShare, "share", "", "Share name for cache API operations (required)")
	cacheTiersCmd.Flags().StringVar(&cacheTiersMount, "mount", "/mnt/test", "Mount point for file I/O")
	cacheTiersCmd.Flags().StringVar(&cacheTiersSizes, "sizes", "", "Comma-separated file sizes (default: 10MB,100MB,1GB)")

	_ = cacheTiersCmd.MarkFlagRequired("share")
}

func runCacheTiers(cmd *cobra.Command, _ []string) error {
	// Get authenticated client for cache operations.
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return fmt.Errorf("authentication required for cache-tiers benchmark: %w", err)
	}

	// Parse file sizes.
	var fileSizes []int64
	if cacheTiersSizes != "" {
		for _, s := range cmdutil.ParseCommaSeparatedList(cacheTiersSizes) {
			size, err := bench.ParseSize(s)
			if err != nil {
				return fmt.Errorf("invalid size %q: %w", s, err)
			}
			fileSizes = append(fileSizes, size)
		}
	}

	// Validate mount point.
	info, err := os.Stat(cacheTiersMount)
	if err != nil {
		return fmt.Errorf("mount point %q: %w", cacheTiersMount, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mount point %q is not a directory", cacheTiersMount)
	}

	cfg := bench.CacheTiersConfig{
		MountPoint: cacheTiersMount,
		ShareName:  cacheTiersShare,
		Client:     client,
		FileSizes:  fileSizes,
	}

	bm := bench.NewCacheTiersBenchmark(cfg)

	fmt.Fprintf(os.Stderr, "Cache Tiers Benchmark - Share: %s\n", cacheTiersShare)
	fmt.Fprintf(os.Stderr, "Mount: %s\n", cacheTiersMount)

	// NewCacheTiersBenchmark already applies defaults, so use its resolved sizes.
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
	return cmdutil.PrintResource(os.Stdout, result, CacheTiersTable{Result: result})
}

// CacheTiersTable renders cache-tiers benchmark results as a table.
type CacheTiersTable struct {
	Result *bench.CacheTiersResult
}

// Headers implements output.TableRenderer.
func (t CacheTiersTable) Headers() []string {
	return []string{"FILE SIZE", "STEP", "THROUGHPUT", "DURATION", "L1 HIT RATE"}
}

// Rows implements output.TableRenderer.
func (t CacheTiersTable) Rows() [][]string {
	var rows [][]string

	for _, r := range t.Result.Results {
		sizeLabel := bench.FormatSize(r.FileSize)

		rows = append(rows,
			formatCacheTiersRow(sizeLabel, "Write", r.WriteStats),
			formatCacheTiersRow(sizeLabel, "Cold Read", r.ColdReadStats),
			formatCacheTiersRow(sizeLabel, "Warm Read", r.WarmReadStats),
			formatCacheTiersRow(sizeLabel, "L2 Read", r.L2OnlyStats),
		)
	}

	return rows
}

func formatCacheTiersRow(size, step string, stats bench.IOStats) []string {
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
