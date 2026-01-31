package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmos91/dittofs/cmd/dittofs/commands/backup"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/spf13/cobra"
)

var (
	restoreInput  string
	restoreConfig string
	restoreForce  bool
)

var controlplaneCmd = &cobra.Command{
	Use:   "controlplane",
	Short: "Restore control plane database from backup",
	Long: `Restore the control plane database from a backup file.

IMPORTANT: The DittoFS server must be stopped before restoring.

Supported backup formats:
  - SQLite database files (.db) - restored by replacing the database file
  - PostgreSQL SQL dumps (.sql) - restored using psql
  - JSON exports (.json) - restored via GORM by recreating all records

The restore command auto-detects the backup format based on file content.

Examples:
  # Restore from SQLite backup
  dittofs restore controlplane --input /tmp/controlplane.db

  # Restore from JSON backup
  dittofs restore controlplane --input /tmp/controlplane.json

  # Restore with force (skip confirmation)
  dittofs restore controlplane --input /tmp/backup.db --force`,
	RunE: runControlplaneRestore,
}

func init() {
	controlplaneCmd.Flags().StringVarP(&restoreInput, "input", "i", "", "Input backup file path (required)")
	controlplaneCmd.Flags().StringVar(&restoreConfig, "config", "", "Path to config file")
	controlplaneCmd.Flags().BoolVar(&restoreForce, "force", false, "Skip confirmation prompt")
	_ = controlplaneCmd.MarkFlagRequired("input")
}

func runControlplaneRestore(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Check if backup file exists
	if _, err := os.Stat(restoreInput); os.IsNotExist(err) {
		return fmt.Errorf("backup file not found: %s", restoreInput)
	}

	// Load configuration
	cfg, err := config.MustLoad(restoreConfig)
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

	// Detect backup format
	format, err := detectBackupFormat(restoreInput)
	if err != nil {
		return fmt.Errorf("failed to detect backup format: %w", err)
	}

	// Confirmation prompt
	if !restoreForce {
		fmt.Printf("WARNING: This will replace the current control plane database.\n")
		fmt.Printf("  Database: %s (%s)\n", cfg.Database.Type, getDatabasePath(&cfg.Database))
		fmt.Printf("  Backup:   %s (%s format)\n", restoreInput, format)
		fmt.Printf("\nMake sure the DittoFS server is stopped before proceeding.\n")
		fmt.Printf("\nType 'yes' to continue: ")

		var response string
		if _, err := fmt.Scanln(&response); err != nil || strings.ToLower(response) != "yes" {
			return fmt.Errorf("restore cancelled")
		}
	}

	startTime := time.Now()

	switch format {
	case "sqlite":
		if cfg.Database.Type != store.DatabaseTypeSQLite {
			return fmt.Errorf("cannot restore SQLite backup to %s database", cfg.Database.Type)
		}
		if err := restoreSQLite(restoreInput, cfg.Database.SQLite.Path); err != nil {
			return err
		}
	case "sql":
		if cfg.Database.Type != store.DatabaseTypePostgres {
			return fmt.Errorf("cannot restore PostgreSQL SQL dump to %s database", cfg.Database.Type)
		}
		if err := restorePostgresSQL(ctx, &cfg.Database.Postgres, restoreInput); err != nil {
			return err
		}
	case "json":
		if err := restoreJSON(ctx, &cfg.Database, restoreInput); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported backup format: %s", format)
	}

	duration := time.Since(startTime)
	fmt.Printf("\nRestore completed successfully\n")
	fmt.Printf("  Source:   %s\n", restoreInput)
	fmt.Printf("  Format:   %s\n", format)
	fmt.Printf("  Target:   %s\n", getDatabasePath(&cfg.Database))
	fmt.Printf("  Duration: %s\n", duration.Round(time.Millisecond))

	return nil
}

// detectBackupFormat determines the format of the backup file.
func detectBackupFormat(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	// Read first few bytes to detect format
	header := make([]byte, 16)
	n, err := file.Read(header)
	if err != nil && err != io.EOF {
		return "", err
	}
	header = header[:n]

	// SQLite database starts with "SQLite format 3"
	if strings.HasPrefix(string(header), "SQLite format 3") {
		return "sqlite", nil
	}

	// JSON starts with { or [
	if len(header) > 0 && (header[0] == '{' || header[0] == '[') {
		return "json", nil
	}

	// PostgreSQL dump starts with "-- PostgreSQL" or similar SQL comments
	if strings.HasPrefix(string(header), "--") || strings.HasPrefix(string(header), "/*") {
		return "sql", nil
	}

	// Check file extension as fallback
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".db", ".sqlite", ".sqlite3":
		return "sqlite", nil
	case ".sql":
		return "sql", nil
	case ".json":
		return "json", nil
	}

	return "", fmt.Errorf("unable to detect backup format for: %s", path)
}

// restoreSQLite restores a SQLite database by replacing the file.
func restoreSQLite(backupPath, targetPath string) error {
	// Ensure target directory exists
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	// Remove existing database and related files
	for _, ext := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(targetPath + ext)
	}

	// Copy backup to target
	return copyFile(backupPath, targetPath)
}

// restorePostgresSQL restores a PostgreSQL database using psql.
func restorePostgresSQL(_ context.Context, cfg *store.PostgresConfig, backupPath string) error {
	// Check if psql is available
	if _, err := exec.LookPath("psql"); err != nil {
		return fmt.Errorf("psql not found in PATH: please install PostgreSQL client tools")
	}

	// Build psql command
	args := []string{
		"-h", cfg.Host,
		"-p", fmt.Sprintf("%d", cfg.Port),
		"-U", cfg.User,
		"-d", cfg.Database,
		"-f", backupPath,
		"--no-password",
	}

	cmd := exec.Command("psql", args...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", cfg.Password))

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("psql restore failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

// restoreJSON restores the database from a JSON backup.
func restoreJSON(ctx context.Context, cfg *store.Config, backupPath string) error {
	// Read backup file
	file, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("failed to open backup file: %w", err)
	}
	defer func() { _ = file.Close() }()

	var backupData backup.ControlPlaneBackup
	if err := json.NewDecoder(file).Decode(&backupData); err != nil {
		return fmt.Errorf("failed to parse JSON backup: %w", err)
	}

	fmt.Printf("Restoring from JSON backup (version %s, created %s)\n", backupData.Version, backupData.Timestamp)

	// For SQLite, remove existing database first
	if cfg.Type == store.DatabaseTypeSQLite {
		for _, ext := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(cfg.SQLite.Path + ext)
		}
	}

	// Create new store (will create fresh schema)
	cpStore, err := store.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to create database: %w", err)
	}
	defer func() { _ = cpStore.Close() }()

	// Restore groups first (users reference groups)
	fmt.Printf("  Restoring %d groups...\n", len(backupData.Groups))
	for _, g := range backupData.Groups {
		group := &models.Group{
			ID:          g.ID,
			Name:        g.Name,
			GID:         g.GID,
			Description: g.Description,
		}
		if _, err := cpStore.CreateGroup(ctx, group); err != nil {
			return fmt.Errorf("failed to restore group %s: %w", g.Name, err)
		}
	}

	// Restore users
	fmt.Printf("  Restoring %d users...\n", len(backupData.Users))
	for _, u := range backupData.Users {
		user := &models.User{
			ID:                 u.ID,
			Username:           u.Username,
			Enabled:            u.Enabled,
			MustChangePassword: true, // Force password change since we don't have the hash
			Role:               u.Role,
			UID:                u.UID,
			GID:                u.GID,
			DisplayName:        u.DisplayName,
			Email:              u.Email,
			PasswordHash:       "", // Will need to be reset
		}
		if _, err := cpStore.CreateUser(ctx, user); err != nil {
			return fmt.Errorf("failed to restore user %s: %w", u.Username, err)
		}

		// Add user to groups
		for _, groupName := range u.Groups {
			if err := cpStore.AddUserToGroup(ctx, u.Username, groupName); err != nil {
				return fmt.Errorf("failed to add user %s to group %s: %w", u.Username, groupName, err)
			}
		}
	}

	// Restore metadata stores
	fmt.Printf("  Restoring %d metadata stores...\n", len(backupData.MetadataStores))
	for _, s := range backupData.MetadataStores {
		if _, err := cpStore.CreateMetadataStore(ctx, s); err != nil {
			return fmt.Errorf("failed to restore metadata store %s: %w", s.Name, err)
		}
	}

	// Restore payload stores
	fmt.Printf("  Restoring %d payload stores...\n", len(backupData.PayloadStores))
	for _, s := range backupData.PayloadStores {
		if _, err := cpStore.CreatePayloadStore(ctx, s); err != nil {
			return fmt.Errorf("failed to restore payload store %s: %w", s.Name, err)
		}
	}

	// Restore shares
	fmt.Printf("  Restoring %d shares...\n", len(backupData.Shares))
	for _, s := range backupData.Shares {
		if _, err := cpStore.CreateShare(ctx, s.Share); err != nil {
			return fmt.Errorf("failed to restore share %s: %w", s.Name, err)
		}

		// Restore access rules
		if len(s.AccessRules) > 0 {
			if err := cpStore.SetShareAccessRules(ctx, s.Name, s.AccessRules); err != nil {
				return fmt.Errorf("failed to restore access rules for share %s: %w", s.Name, err)
			}
		}
	}

	// Restore user share permissions
	for _, u := range backupData.Users {
		for _, perm := range u.SharePermissions {
			if err := cpStore.SetUserSharePermission(ctx, perm); err != nil {
				return fmt.Errorf("failed to restore permission for user %s on share %s: %w",
					u.Username, perm.ShareName, err)
			}
		}
	}

	// Restore group share permissions
	for _, g := range backupData.Groups {
		for _, perm := range g.SharePermissions {
			if err := cpStore.SetGroupSharePermission(ctx, perm); err != nil {
				return fmt.Errorf("failed to restore permission for group %s on share %s: %w",
					g.Name, perm.ShareName, err)
			}
		}
	}

	// Restore adapters
	fmt.Printf("  Restoring %d adapters...\n", len(backupData.Adapters))
	for _, a := range backupData.Adapters {
		if _, err := cpStore.CreateAdapter(ctx, a); err != nil {
			return fmt.Errorf("failed to restore adapter %s: %w", a.Type, err)
		}
	}

	// Restore settings
	fmt.Printf("  Restoring %d settings...\n", len(backupData.Settings))
	for _, s := range backupData.Settings {
		if err := cpStore.SetSetting(ctx, s.Key, s.Value); err != nil {
			return fmt.Errorf("failed to restore setting %s: %w", s.Key, err)
		}
	}

	fmt.Println("\nNote: Users restored from JSON backup will need to reset their passwords.")

	return nil
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer func() { _ = source.Close() }()

	dest, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer func() { _ = dest.Close() }()

	if _, err := io.Copy(dest, source); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	return dest.Sync()
}

// getDatabasePath returns a human-readable path for the database.
func getDatabasePath(cfg *store.Config) string {
	switch cfg.Type {
	case store.DatabaseTypeSQLite:
		return cfg.SQLite.Path
	case store.DatabaseTypePostgres:
		return fmt.Sprintf("%s@%s:%d/%s", cfg.Postgres.User, cfg.Postgres.Host, cfg.Postgres.Port, cfg.Postgres.Database)
	default:
		return string(cfg.Type)
	}
}
