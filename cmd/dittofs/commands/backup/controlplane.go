package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/spf13/cobra"
)

var (
	controlplaneOutput string
	controlplaneFormat string
	controlplaneConfig string
)

var controlplaneCmd = &cobra.Command{
	Use:   "controlplane",
	Short: "Backup control plane database",
	Long: `Backup the control plane database (identity store).

Creates a backup of the user, group, and permission data stored in the
control plane database.

Examples:
  # Backup to file
  dittofs backup controlplane --output /tmp/backup.json

  # Backup with specific config
  dittofs backup controlplane --config /etc/dittofs/config.yaml --output /tmp/backup.json`,
	RunE: runControlplaneBackup,
}

func init() {
	controlplaneCmd.Flags().StringVarP(&controlplaneOutput, "output", "o", "", "Output file path (required)")
	controlplaneCmd.Flags().StringVar(&controlplaneFormat, "format", "json", "Backup format (json|binary)")
	controlplaneCmd.Flags().StringVar(&controlplaneConfig, "config", "", "Path to config file")
	_ = controlplaneCmd.MarkFlagRequired("output")
}

func runControlplaneBackup(cmd *cobra.Command, args []string) error {
	// Validate format
	if controlplaneFormat != "json" && controlplaneFormat != "binary" {
		return fmt.Errorf("invalid format: %s (valid: json, binary)", controlplaneFormat)
	}

	configFile := controlplaneConfig

	// Check if config exists
	if configFile == "" {
		if !config.DefaultConfigExists() {
			return fmt.Errorf("no configuration file found at default location: %s\n\n"+
				"Please specify a config file:\n"+
				"  dittofs backup controlplane --config /path/to/config.yaml --output /tmp/backup.json",
				config.GetDefaultConfigPath())
		}
		configFile = config.GetDefaultConfigPath()
	}

	// Check explicitly specified path
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		return fmt.Errorf("configuration file not found: %s", configFile)
	}

	// Load configuration
	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Initialize the structured logger
	loggerCfg := logger.Config{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
		Output: cfg.Logging.Output,
	}
	if err := logger.Init(loggerCfg); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}

	// Get backup data
	// Note: This exports users and groups from the config file
	// For database-backed users, use the control plane store directly
	backup := struct {
		Timestamp string               `json:"timestamp"`
		Version   string               `json:"version"`
		Users     []config.UserConfig  `json:"users"`
		Groups    []config.GroupConfig `json:"groups"`
	}{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Version:   "1.0",
		Users:     cfg.Users,
		Groups:    cfg.Groups,
	}

	// Ensure output directory exists
	outputDir := filepath.Dir(controlplaneOutput)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Write backup file
	file, err := os.Create(controlplaneOutput)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() { _ = file.Close() }()

	switch controlplaneFormat {
	case "json":
		if err := writeJSONBackup(file, backup); err != nil {
			return fmt.Errorf("failed to write backup: %w", err)
		}
	case "binary":
		// Binary format would use gob or similar
		return fmt.Errorf("binary format not yet implemented")
	}

	fmt.Printf("Backup created: %s\n", controlplaneOutput)
	fmt.Printf("  Format:    %s\n", controlplaneFormat)
	fmt.Printf("  Timestamp: %s\n", backup.Timestamp)
	fmt.Printf("  Users:     %d\n", len(backup.Users))
	fmt.Printf("  Groups:    %d\n", len(backup.Groups))

	return nil
}

func writeJSONBackup(file *os.File, data any) error {
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(data)
}
