package commands

import (
	"path/filepath"

	"github.com/marmos91/dittofs/pkg/config"
)

// InitLogger initializes the structured logger from configuration.
// Delegates to config.InitLogger to ensure rotation settings are plumbed through.
func InitLogger(cfg *config.Config) error {
	return config.InitLogger(cfg)
}

// GetDefaultStateDir returns the default state directory path.
func GetDefaultStateDir() string {
	return config.GetStateDir()
}

// GetDefaultPidFile returns the default PID file path.
func GetDefaultPidFile() string {
	return filepath.Join(GetDefaultStateDir(), "dittofs.pid")
}

// GetDefaultLogFile returns the default log file path for daemon mode.
func GetDefaultLogFile() string {
	return filepath.Join(GetDefaultStateDir(), "dittofs.log")
}
