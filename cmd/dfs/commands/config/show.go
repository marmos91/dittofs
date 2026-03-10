package config

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/internal/sysinfo"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var (
	showOutput  string
	showDeduced bool
)

var showCmd = &cobra.Command{
	Use:   "show",
	Short: "Display current configuration",
	Long: `Display the current DittoFS configuration.

By default outputs YAML format. Use --output to change format.
Use --deduced to show auto-deduced block store defaults based on system resources.

Examples:
  # Show default config as YAML
  dfs config show

  # Show as JSON
  dfs config show --output json

  # Show specific config file
  dfs config show --config /etc/dittofs/config.yaml

  # Show auto-deduced block store defaults
  dfs config show --deduced`,
	RunE: runConfigShow,
}

func init() {
	showCmd.Flags().StringVarP(&showOutput, "output", "o", "yaml", "Output format (yaml|json)")
	showCmd.Flags().BoolVar(&showDeduced, "deduced", false, "Show auto-deduced block store defaults based on system resources")
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	if showDeduced {
		return runShowDeduced()
	}

	// Get config path from parent's persistent flag
	configPath, _ := cmd.Flags().GetString("config")

	// Load configuration
	cfg, err := config.MustLoad(configPath)
	if err != nil {
		return err
	}

	// Parse output format
	format, err := output.ParseFormat(showOutput)
	if err != nil {
		return err
	}

	// Print configuration
	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, cfg)
	default:
		return output.PrintYAML(os.Stdout, cfg)
	}
}

// runShowDeduced displays auto-deduced block store defaults with system info.
func runShowDeduced() error {
	detector := sysinfo.NewDetector()
	deduced := blockstore.DeduceDefaults(detector)

	mem := blockstore.FormatBytes(detector.AvailableMemory())

	fmt.Printf("# System Resources\n")
	fmt.Printf("# CPUs: %d (source: runtime.GOMAXPROCS)\n", detector.AvailableCPUs())
	fmt.Printf("# Memory: %s (source: %s)\n\n", mem, detector.MemorySource())

	fmt.Printf("# Auto-Deduced Block Store Defaults (per share)\n")
	fmt.Printf("# These values are used when shares don't specify explicit overrides.\n\n")

	fmt.Printf("local_store_size: %s  # 25%% of %s\n",
		blockstore.FormatBytes(deduced.LocalStoreSize), mem)
	fmt.Printf("l1_cache_size: %s  # 12.5%% of %s\n",
		blockstore.FormatBytes(uint64(deduced.L1CacheSize)), mem)
	fmt.Printf("max_pending_size: %s  # 50%% of local_store_size\n",
		blockstore.FormatBytes(deduced.MaxPendingSize))
	fmt.Printf("parallel_syncs: %d  # max(4, %d CPUs)\n",
		deduced.ParallelSyncs, detector.AvailableCPUs())
	fmt.Printf("parallel_fetches: %d  # max(8, %d CPUs * 2)\n",
		deduced.ParallelFetches, detector.AvailableCPUs())
	fmt.Printf("prefetch_workers: %d  # fixed default\n", deduced.PrefetchWorkers)

	return nil
}
