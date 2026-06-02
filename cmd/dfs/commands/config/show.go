package config

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/internal/sysinfo"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
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
		// The Config structs carry only mapstructure/yaml tags (no json
		// tags), so a direct json.Marshal emits PascalCase keys that
		// config.Load cannot re-parse. Round-trip through yaml first to
		// obtain the lowercase yaml-keyed shape (which also applies the
		// secret-redaction MarshalYAML hooks), then emit that as JSON so
		// the output re-parses through Load.
		keyed, err := yamlKeyedView(cfg)
		if err != nil {
			return err
		}
		return output.PrintJSON(os.Stdout, keyed)
	default:
		return output.PrintYAML(os.Stdout, cfg)
	}
}

// yamlKeyedView converts cfg to a generic map keyed by its yaml tags by
// marshalling to YAML and unmarshalling back into an interface{}. This yields
// the same lowercase key namespace config.Load consumes, so the JSON encoding
// of the result round-trips through Load.
func yamlKeyedView(cfg *config.Config) (interface{}, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}
	var view interface{}
	if err := yaml.Unmarshal(data, &view); err != nil {
		return nil, fmt.Errorf("failed to re-key config: %w", err)
	}
	return view, nil
}

// runShowDeduced displays auto-deduced block store defaults with system info.
func runShowDeduced() error {
	detector := sysinfo.NewDetector()
	deduced := block.DeduceDefaults(detector)

	mem := block.FormatBytes(detector.AvailableMemory())

	fmt.Printf("# System Resources\n")
	fmt.Printf("# CPUs: %d (source: runtime.GOMAXPROCS)\n", detector.AvailableCPUs())
	fmt.Printf("# Memory: %s (source: %s)\n\n", mem, detector.MemorySource())

	fmt.Printf("# Auto-Deduced Block Store Defaults (per share)\n")
	fmt.Printf("# These values are used when shares don't specify explicit overrides.\n\n")

	fmt.Printf("local_store_size: %s  # 25%% of %s\n",
		block.FormatBytes(deduced.LocalStoreSize), mem)
	fmt.Printf("read_buffer_size: %s  # 12.5%% of %s\n",
		block.FormatBytes(uint64(deduced.ReadBufferSize)), mem)
	fmt.Printf("max_pending_size: %s  # 50%% of local_store_size\n",
		block.FormatBytes(deduced.MaxPendingSize))
	fmt.Printf("parallel_syncs: %d  # max(4, %d CPUs)\n",
		deduced.ParallelSyncs, detector.AvailableCPUs())
	fmt.Printf("parallel_fetches: %d  # max(8, %d CPUs * 2)\n",
		deduced.ParallelFetches, detector.AvailableCPUs())
	fmt.Printf("prefetch_workers: %d  # fixed default\n", deduced.PrefetchWorkers)

	return nil
}
