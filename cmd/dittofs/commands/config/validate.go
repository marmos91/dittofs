package config

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var validateConfigPath string

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate configuration file",
	Long: `Validate the DittoFS configuration file.

Checks for syntax errors, missing required fields, and invalid values.

Examples:
  # Validate default config
  dittofs config validate

  # Validate specific config file
  dittofs config validate --config /etc/dittofs/config.yaml`,
	RunE: runConfigValidate,
}

func init() {
	validateCmd.Flags().StringVar(&validateConfigPath, "config", "", "Path to config file")
}

func runConfigValidate(cmd *cobra.Command, args []string) error {
	configPath := validateConfigPath
	if configPath == "" {
		if !config.DefaultConfigExists() {
			return fmt.Errorf("no configuration file found at default location: %s\n\n"+
				"Create one with:\n"+
				"  dittofs config init",
				config.GetDefaultConfigPath())
		}
		configPath = config.GetDefaultConfigPath()
	}

	// Check if file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return fmt.Errorf("configuration file not found: %s", configPath)
	}

	// Try to load and validate configuration
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Additional validation checks
	var warnings []string

	// Check for at least one share
	if len(cfg.Shares) == 0 {
		warnings = append(warnings, "No shares configured")
	}

	// Check for at least one adapter
	adapterCount := 0
	if cfg.Adapters.NFS.Enabled {
		adapterCount++
	}
	if cfg.Adapters.SMB.Enabled {
		adapterCount++
	}
	if adapterCount == 0 {
		warnings = append(warnings, "No protocol adapters enabled")
	}

	// Check metadata stores referenced by shares exist
	for _, share := range cfg.Shares {
		if _, exists := cfg.Metadata.Stores[share.Metadata]; !exists {
			warnings = append(warnings, fmt.Sprintf("Share '%s' references non-existent metadata store '%s'", share.Name, share.Metadata))
		}
		if share.Payload != "" {
			if _, exists := cfg.Payload.Stores[share.Payload]; !exists {
				warnings = append(warnings, fmt.Sprintf("Share '%s' references non-existent payload store '%s'", share.Name, share.Payload))
			}
		}
	}

	// Print results
	fmt.Printf("Configuration file: %s\n", configPath)
	fmt.Println("Validation: OK")

	if len(warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range warnings {
			fmt.Printf("  - %s\n", w)
		}
	}

	fmt.Printf("\nConfiguration summary:\n")
	fmt.Printf("  Shares:          %d\n", len(cfg.Shares))
	fmt.Printf("  Metadata stores: %d\n", len(cfg.Metadata.Stores))
	fmt.Printf("  Payload stores:  %d\n", len(cfg.Payload.Stores))
	fmt.Printf("  NFS enabled:     %v\n", cfg.Adapters.NFS.Enabled)
	fmt.Printf("  SMB enabled:     %v\n", cfg.Adapters.SMB.Enabled)
	fmt.Printf("  API enabled:     %v\n", cfg.Server.API.IsEnabled())

	return nil
}
