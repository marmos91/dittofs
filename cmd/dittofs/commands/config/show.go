package config

import (
	"os"

	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var (
	showConfigPath string
	showOutput     string
)

var showCmd = &cobra.Command{
	Use:   "show",
	Short: "Display current configuration",
	Long: `Display the current DittoFS configuration.

By default outputs YAML format. Use --output to change format.

Examples:
  # Show default config as YAML
  dittofs config show

  # Show as JSON
  dittofs config show --output json

  # Show specific config file
  dittofs config show --config /etc/dittofs/config.yaml`,
	RunE: runConfigShow,
}

func init() {
	showCmd.Flags().StringVar(&showConfigPath, "config", "", "Path to config file")
	showCmd.Flags().StringVarP(&showOutput, "output", "o", "yaml", "Output format (yaml|json)")
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.MustLoad(showConfigPath)
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
