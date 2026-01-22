package commands

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations",
	Long: `Run database migrations for PostgreSQL metadata stores.

This command applies pending database migrations to the configured PostgreSQL
metadata store. It is required after upgrading DittoFS when schema changes
have been made.

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

	// Find PostgreSQL metadata store configuration
	var postgresCfg *postgres.PostgresMetadataStoreConfig
	for name, storeCfg := range cfg.Metadata.Stores {
		if storeCfg.Type == "postgres" {
			var pgCfg postgres.PostgresMetadataStoreConfig
			if err := mapstructure.Decode(storeCfg.Postgres, &pgCfg); err != nil {
				return fmt.Errorf("invalid postgres config: %w", err)
			}
			pgCfg.ApplyDefaults()
			postgresCfg = &pgCfg
			logger.Info("Found PostgreSQL metadata store", "name", name)
			break
		}
	}

	if postgresCfg == nil {
		return fmt.Errorf("no PostgreSQL metadata store configured\n\n" +
			"To use migrations, configure a PostgreSQL metadata store in your config:\n\n" +
			"metadata:\n" +
			"  stores:\n" +
			"    postgres:\n" +
			"      type: postgres\n" +
			"      postgres:\n" +
			"        host: localhost\n" +
			"        port: 5432\n" +
			"        database: dittofs\n" +
			"        user: postgres\n" +
			"        password: secret")
	}

	// Run migrations
	ctx := context.Background()
	fmt.Println("Running PostgreSQL database migrations...")

	if err := postgres.RunMigrations(ctx, postgresCfg); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}

	fmt.Println("Migrations completed successfully")
	return nil
}
