package config

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var showOutput string

var showCmd = &cobra.Command{
	Use:   "show",
	Short: "Display current configuration",
	Long: `Display the current DittoFS configuration.

By default outputs YAML format. Use --output to change format.

Examples:
  # Show default config as YAML
  dfs config show

  # Show as JSON
  dfs config show --output json

  # Show specific config file
  dfs config show --config /etc/dittofs/config.yaml`,
	RunE: runConfigShow,
}

func init() {
	showCmd.Flags().StringVarP(&showOutput, "output", "o", "yaml", "Output format (yaml|json)")
}

func runConfigShow(cmd *cobra.Command, args []string) error {
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
