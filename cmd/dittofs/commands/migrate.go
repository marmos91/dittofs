package commands

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations",
	Long: `Run database migrations for the control plane database.

This command applies pending database migrations to the configured control plane
database (SQLite or PostgreSQL). It is required after upgrading DittoFS when
schema changes have been made.

Examples:
  # Run migrations with default config
  dittofs migrate

  # Run migrations with custom config
  dittofs migrate --config /etc/dittofs/config.yaml`,
	RunE: runMigrate,
}

func runMigrate(cmd *cobra.Command, args []string) error {
	cfg, err := config.MustLoad(GetConfigFile())
	if err != nil {
		return err
	}

	// Initialize the structured logger
	if err := InitLogger(cfg); err != nil {
		return err
	}

	logger.Info("Running database migrations", "type", cfg.Database.Type)

	// Create control plane store (this triggers auto-migration)
	ctx := context.Background()
	cpStore, err := store.New(&cfg.Database)
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	defer func() { _ = cpStore.Close() }()

	// Verify the migration worked by checking if we can query users
	_, err = cpStore.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("migration verification failed: %w", err)
	}

	fmt.Printf("Migrations completed successfully (database type: %s)\n", cfg.Database.Type)
	return nil
}
