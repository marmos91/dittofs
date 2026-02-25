package config

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

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

func runConfigValidate(cmd *cobra.Command, args []string) error {
	// Get config path from parent's persistent flag
	configPath, _ := cmd.Flags().GetString("config")

	// Load and validate configuration
	cfg, err := config.MustLoad(configPath)
	if err != nil {
		return err
	}

	// Determine path for display
	displayPath := configPath
	if displayPath == "" {
		displayPath = config.GetDefaultConfigPath()
	}

	// Additional validation checks
	var warnings []string

	// Check JWT secret is configured
	if !cfg.ControlPlane.HasJWTSecret() {
		warnings = append(warnings, "JWT secret not configured - API authentication will fail")
	}

	// Check cache path is set
	if cfg.Cache.Path == "" {
		warnings = append(warnings, "Cache path not configured")
	}

	// Print results
	fmt.Printf("Configuration file: %s\n", displayPath)
	fmt.Println("Validation: OK")

	if len(warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range warnings {
			fmt.Printf("  - %s\n", w)
		}
	}

	fmt.Printf("\nConfiguration summary:\n")
	fmt.Printf("  Database type:   %s\n", cfg.Database.Type)
	fmt.Printf("  API port:        %d\n", cfg.ControlPlane.Port)
	fmt.Printf("  Log level:       %s\n", cfg.Logging.Level)

	return nil
}
