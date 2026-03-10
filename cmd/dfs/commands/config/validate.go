package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"

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
  dfs config validate

  # Validate specific config file
  dfs config validate --config /etc/dittofs/config.yaml`,
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

	// Check for legacy 'payload:' YAML key in config file
	if legacyWarnings := checkLegacyPayloadKey(displayPath); len(legacyWarnings) > 0 {
		warnings = append(warnings, legacyWarnings...)
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

// checkLegacyPayloadKey scans a config file for the deprecated 'payload:' YAML key.
// Returns warnings if legacy keys are found.
func checkLegacyPayloadKey(configPath string) []string {
	f, err := os.Open(configPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var warnings []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		// Detect top-level or nested 'payload:' or 'payload_store:' keys
		if strings.HasPrefix(trimmed, "payload:") || strings.HasPrefix(trimmed, "payload_store:") {
			warnings = append(warnings, fmt.Sprintf(
				"Line %d: Config key '%s' has been renamed to 'block_store:'. "+
					"Please update your config file. See docs/CONFIGURATION.md for the new format.",
				lineNum, strings.TrimSuffix(strings.TrimSpace(trimmed), ":")))
		}
	}
	return warnings
}
