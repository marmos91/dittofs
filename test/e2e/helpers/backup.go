//go:build e2e

package helpers

import (
	"encoding/json"
	"os"
	"testing"
)

// =============================================================================
// Backup Types and Functions
// =============================================================================

// ControlPlaneBackup represents the structure of a JSON backup file.
// This matches the structure from cmd/dfs/commands/backup/controlplane.go.
type ControlPlaneBackup struct {
	Timestamp      string            `json:"timestamp"`
	Version        string            `json:"version"`
	DatabaseType   string            `json:"database_type"`
	Users          []BackupUser      `json:"users"`
	Groups         []BackupGroup     `json:"groups"`
	Shares         []json.RawMessage `json:"shares"` // Raw to avoid deep model dependency
	MetadataStores []json.RawMessage `json:"metadata_stores"`
	PayloadStores  []json.RawMessage `json:"payload_stores"`
	Adapters       []json.RawMessage `json:"adapters"`
	Settings       []json.RawMessage `json:"settings"`
}

// BackupUser represents a user in a backup file.
type BackupUser struct {
	ID                 string   `json:"id"`
	Username           string   `json:"username"`
	Role               string   `json:"role"`
	Enabled            bool     `json:"enabled"`
	MustChangePassword bool     `json:"must_change_password"`
	UID                *uint32  `json:"uid,omitempty"`
	GID                *uint32  `json:"gid,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	Email              string   `json:"email,omitempty"`
	Groups             []string `json:"groups,omitempty"`
}

// BackupGroup represents a group in a backup file.
type BackupGroup struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	GID         *uint32 `json:"gid,omitempty"`
	Description string  `json:"description,omitempty"`
}

// RunDfsBackup runs the dfs backup controlplane command.
// outputPath: where to write the backup file
// configPath: path to server config (needed to locate the database)
// format: backup format - "native", "native-cli", or "json" (empty = default "native")
func RunDfsBackup(t *testing.T, outputPath, configPath, format string) error {
	t.Helper()

	args := []string{"backup", "controlplane",
		"--output", outputPath,
		"--config", configPath,
	}
	if format != "" {
		args = append(args, "--format", format)
	}
	_, err := RunDfs(t, args...)
	return err
}

// ParseBackupFile reads and parses a JSON backup file.
// Returns the parsed ControlPlaneBackup structure for verification.
func ParseBackupFile(t *testing.T, path string) (*ControlPlaneBackup, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var backup ControlPlaneBackup
	if err := json.Unmarshal(data, &backup); err != nil {
		return nil, err
	}
	return &backup, nil
}
