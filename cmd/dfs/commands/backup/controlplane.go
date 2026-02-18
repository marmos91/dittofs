package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/spf13/cobra"
)

var (
	controlplaneOutput string
	controlplaneConfig string
	controlplaneFormat string
)

var controlplaneCmd = &cobra.Command{
	Use:   "controlplane",
	Short: "Backup control plane database",
	Long: `Backup the control plane database.

For SQLite databases:
  Creates a backup using VACUUM INTO (pure Go, no external tools needed).
  Can optionally use sqlite3 CLI with --format=native-cli for hot backups.

For PostgreSQL databases:
  Uses pg_dump if available, otherwise falls back to JSON export.

Formats:
  native      Use VACUUM INTO for SQLite (pure Go), pg_dump for PostgreSQL
  native-cli  Use sqlite3/pg_dump CLI tools (requires tools to be installed)
  json        Export as JSON via GORM (portable, works for all backends)

Examples:
  # Backup SQLite database (pure Go, recommended)
  dittofs backup controlplane --output /tmp/controlplane.db

  # Backup using native CLI tools
  dittofs backup controlplane --format native-cli --output /tmp/controlplane.db

  # Backup as JSON (works for both backends)
  dittofs backup controlplane --format json --output /tmp/controlplane.json`,
	RunE: runControlplaneBackup,
}

func init() {
	controlplaneCmd.Flags().StringVarP(&controlplaneOutput, "output", "o", "", "Output file path (required)")
	controlplaneCmd.Flags().StringVar(&controlplaneConfig, "config", "", "Path to config file")
	controlplaneCmd.Flags().StringVar(&controlplaneFormat, "format", "native", "Backup format: native, native-cli, or json")
	_ = controlplaneCmd.MarkFlagRequired("output")
}

func runControlplaneBackup(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Validate format
	switch controlplaneFormat {
	case "native", "native-cli", "json":
		// valid
	default:
		return fmt.Errorf("invalid format: %s (valid: native, native-cli, json)", controlplaneFormat)
	}

	// Load configuration
	cfg, err := config.MustLoad(controlplaneConfig)
	if err != nil {
		return err
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

	// Apply defaults to database config
	cfg.Database.ApplyDefaults()

	// Ensure output directory exists
	outputDir := filepath.Dir(controlplaneOutput)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	startTime := time.Now()
	actualFormat := controlplaneFormat

	switch controlplaneFormat {
	case "json":
		if err := backupJSON(ctx, &cfg.Database, controlplaneOutput); err != nil {
			return err
		}
	case "native-cli":
		switch cfg.Database.Type {
		case store.DatabaseTypeSQLite:
			if err := backupSQLiteCLI(cfg.Database.SQLite.Path, controlplaneOutput); err != nil {
				return err
			}
			actualFormat = "sqlite-cli"
		case store.DatabaseTypePostgres:
			// Check pg_dump availability before backup to set correct format
			if _, err := exec.LookPath("pg_dump"); err != nil {
				actualFormat = "json"
			} else {
				actualFormat = "pg_dump"
			}
			if err := backupPostgresCLI(ctx, &cfg.Database, controlplaneOutput); err != nil {
				return err
			}
		}
	case "native":
		switch cfg.Database.Type {
		case store.DatabaseTypeSQLite:
			if err := backupSQLiteNative(ctx, &cfg.Database, controlplaneOutput); err != nil {
				return err
			}
			actualFormat = "sqlite"
		case store.DatabaseTypePostgres:
			// PostgreSQL doesn't have a pure Go backup method, fall back to pg_dump or JSON
			if _, err := exec.LookPath("pg_dump"); err == nil {
				if err := backupPostgresCLI(ctx, &cfg.Database, controlplaneOutput); err != nil {
					return err
				}
				actualFormat = "pg_dump"
			} else {
				fmt.Println("Note: pg_dump not found, using JSON export")
				if err := backupJSON(ctx, &cfg.Database, controlplaneOutput); err != nil {
					return err
				}
				actualFormat = "json"
			}
		default:
			return fmt.Errorf("unsupported database type: %s", cfg.Database.Type)
		}
	}

	// Get file size
	stat, err := os.Stat(controlplaneOutput)
	if err != nil {
		return fmt.Errorf("failed to stat output file: %w", err)
	}

	duration := time.Since(startTime)
	fmt.Printf("Backup completed successfully\n")
	fmt.Printf("  Output:   %s\n", controlplaneOutput)
	fmt.Printf("  Type:     %s\n", cfg.Database.Type)
	fmt.Printf("  Format:   %s\n", actualFormat)
	fmt.Printf("  Size:     %s\n", formatBytes(stat.Size()))
	fmt.Printf("  Duration: %s\n", duration.Round(time.Millisecond))

	return nil
}

// backupSQLiteNative creates a backup using VACUUM INTO (pure Go, no CLI needed).
func backupSQLiteNative(_ context.Context, cfg *store.Config, outputPath string) error {
	// Check if source database exists before attempting backup.
	// This prevents store.New() from creating a new empty database.
	if _, err := os.Stat(cfg.SQLite.Path); os.IsNotExist(err) {
		return fmt.Errorf("source database not found: %s", cfg.SQLite.Path)
	}

	cpStore, err := store.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = cpStore.Close() }()

	// Use VACUUM INTO to create a backup (available since SQLite 3.27.0)
	// This is safe to run while the database is in use
	sql := fmt.Sprintf("VACUUM INTO '%s'", outputPath)
	if err := cpStore.DB().Exec(sql).Error; err != nil {
		return fmt.Errorf("VACUUM INTO failed: %w", err)
	}

	return nil
}

// backupSQLiteCLI creates a backup using sqlite3 CLI for hot backup.
func backupSQLiteCLI(dbPath, outputPath string) error {
	// Check if source database exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return fmt.Errorf("source database not found: %s", dbPath)
	}

	// Check if sqlite3 CLI is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return fmt.Errorf("sqlite3 CLI not found: please install sqlite3 or use --format=native")
	}

	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf(".backup '%s'", outputPath))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sqlite3 backup failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

// backupPostgresCLI creates a backup using pg_dump, falls back to JSON if not available.
func backupPostgresCLI(ctx context.Context, cfg *store.Config, outputPath string) error {
	// Check if pg_dump is available
	if _, err := exec.LookPath("pg_dump"); err != nil {
		fmt.Println("Warning: pg_dump not found, falling back to JSON export")
		return backupJSON(ctx, cfg, outputPath)
	}

	// Build pg_dump command
	args := []string{
		"-h", cfg.Postgres.Host,
		"-p", fmt.Sprintf("%d", cfg.Postgres.Port),
		"-U", cfg.Postgres.User,
		"-d", cfg.Postgres.Database,
		"-f", outputPath,
		"--no-password", // Expect PGPASSWORD env var or .pgpass
	}

	cmd := exec.Command("pg_dump", args...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", cfg.Postgres.Password))

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pg_dump failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

// backupJSON creates a JSON export of the control plane data via GORM.
// This is portable and works without external database tools.
func backupJSON(ctx context.Context, cfg *store.Config, outputPath string) error {
	cpStore, err := store.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = cpStore.Close() }()

	backup, err := exportControlPlane(ctx, cpStore)
	if err != nil {
		return fmt.Errorf("failed to export data: %w", err)
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() { _ = file.Close() }()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(backup); err != nil {
		return fmt.Errorf("failed to write JSON: %w", err)
	}

	return nil
}

// ControlPlaneBackup represents a full export of the control plane database.
type ControlPlaneBackup struct {
	Timestamp      string                        `json:"timestamp"`
	Version        string                        `json:"version"`
	DatabaseType   string                        `json:"database_type"`
	Users          []UserBackup                  `json:"users"`
	Groups         []GroupBackup                 `json:"groups"`
	Shares         []ShareBackup                 `json:"shares"`
	MetadataStores []*models.MetadataStoreConfig `json:"metadata_stores"`
	PayloadStores  []*models.PayloadStoreConfig  `json:"payload_stores"`
	Adapters       []*models.AdapterConfig       `json:"adapters"`
	Settings       []*models.Setting             `json:"settings"`
}

// UserBackup represents a user for backup purposes (excludes sensitive fields).
type UserBackup struct {
	ID                 string                        `json:"id"`
	Username           string                        `json:"username"`
	Enabled            bool                          `json:"enabled"`
	MustChangePassword bool                          `json:"must_change_password"`
	Role               string                        `json:"role"`
	UID                *uint32                       `json:"uid,omitempty"`
	GID                *uint32                       `json:"gid,omitempty"`
	DisplayName        string                        `json:"display_name,omitempty"`
	Email              string                        `json:"email,omitempty"`
	Groups             []string                      `json:"groups,omitempty"`
	SharePermissions   []*models.UserSharePermission `json:"share_permissions,omitempty"`
}

// GroupBackup represents a group for backup purposes.
type GroupBackup struct {
	ID               string                         `json:"id"`
	Name             string                         `json:"name"`
	GID              *uint32                        `json:"gid,omitempty"`
	Description      string                         `json:"description,omitempty"`
	SharePermissions []*models.GroupSharePermission `json:"share_permissions,omitempty"`
}

// ShareBackup represents a share for backup purposes.
type ShareBackup struct {
	*models.Share
	AccessRules []*models.ShareAccessRule `json:"access_rules,omitempty"`
}

func exportControlPlane(ctx context.Context, cpStore store.Store) (*ControlPlaneBackup, error) {
	backup := &ControlPlaneBackup{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Version:      "1.0",
		DatabaseType: "json", // Indicates this is a JSON export
	}

	// Export users
	users, err := cpStore.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	for _, u := range users {
		// Get user's groups
		groups, err := cpStore.GetUserGroups(ctx, u.Username)
		if err != nil {
			return nil, fmt.Errorf("failed to get groups for user %s: %w", u.Username, err)
		}
		groupNames := make([]string, len(groups))
		for i, g := range groups {
			groupNames[i] = g.Name
		}

		// Get user's share permissions
		perms, err := cpStore.GetUserSharePermissions(ctx, u.Username)
		if err != nil {
			return nil, fmt.Errorf("failed to get share permissions for user %s: %w", u.Username, err)
		}

		backup.Users = append(backup.Users, UserBackup{
			ID:                 u.ID,
			Username:           u.Username,
			Enabled:            u.Enabled,
			MustChangePassword: u.MustChangePassword,
			Role:               u.Role,
			UID:                u.UID,
			GID:                u.GID,
			DisplayName:        u.DisplayName,
			Email:              u.Email,
			Groups:             groupNames,
			SharePermissions:   perms,
		})
	}

	// Export groups
	groups, err := cpStore.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups: %w", err)
	}
	for _, g := range groups {
		// Get group's share permissions
		perms, err := cpStore.GetGroupSharePermissions(ctx, g.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get share permissions for group %s: %w", g.Name, err)
		}

		backup.Groups = append(backup.Groups, GroupBackup{
			ID:               g.ID,
			Name:             g.Name,
			GID:              g.GID,
			Description:      g.Description,
			SharePermissions: perms,
		})
	}

	// Export shares
	shares, err := cpStore.ListShares(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list shares: %w", err)
	}
	for _, s := range shares {
		// Get share's access rules
		rules, err := cpStore.GetShareAccessRules(ctx, s.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get access rules for share %s: %w", s.Name, err)
		}

		backup.Shares = append(backup.Shares, ShareBackup{
			Share:       s,
			AccessRules: rules,
		})
	}

	// Export metadata stores
	backup.MetadataStores, err = cpStore.ListMetadataStores(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list metadata stores: %w", err)
	}

	// Export payload stores
	backup.PayloadStores, err = cpStore.ListPayloadStores(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list payload stores: %w", err)
	}

	// Export adapters
	backup.Adapters, err = cpStore.ListAdapters(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list adapters: %w", err)
	}

	// Export settings
	backup.Settings, err = cpStore.ListSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list settings: %w", err)
	}

	return backup, nil
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
