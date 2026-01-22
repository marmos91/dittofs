package commands

import (
	"context"
	"fmt"
	"os"

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
	configFile := GetConfigFile()

	// Check if config exists
	if configFile == "" {
		// Check default location
		if !config.DefaultConfigExists() {
			return fmt.Errorf("no configuration file found at default location: %s\n\n"+
				"Please initialize a configuration file first:\n"+
				"  dittofs init\n\n"+
				"Or specify a custom config file:\n"+
				"  dittofs migrate --config /path/to/config.yaml",
				config.GetDefaultConfigPath())
		}
	} else {
		// Check explicitly specified path
		if _, err := os.Stat(configFile); os.IsNotExist(err) {
			return fmt.Errorf("configuration file not found: %s\n\n"+
				"Please create the configuration file:\n"+
				"  dittofs init --config %s",
				configFile, configFile)
		}
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
