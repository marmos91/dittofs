package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/config"
)

// InitLogger initializes the structured logger from configuration.
func InitLogger(cfg *config.Config) error {
	loggerCfg := logger.Config{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
		Output: cfg.Logging.Output,
	}
	if err := logger.Init(loggerCfg); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	return nil
}

// GetDefaultStateDir returns the default state directory path.
func GetDefaultStateDir() string {
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "/tmp"
		}
		stateDir = filepath.Join(homeDir, ".local", "state")
	}
	return filepath.Join(stateDir, "dittofs")
}

// GetDefaultPidFile returns the default PID file path.
func GetDefaultPidFile() string {
	return filepath.Join(GetDefaultStateDir(), "dittofs.pid")
}

// GetDefaultLogFile returns the default log file path for daemon mode.
func GetDefaultLogFile() string {
	return filepath.Join(GetDefaultStateDir(), "dittofs.log")
}
