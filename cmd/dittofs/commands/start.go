package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	dittoServer "github.com/marmos91/dittofs/pkg/server"
	"github.com/spf13/cobra"

	// Import prometheus metrics to register init() functions
	_ "github.com/marmos91/dittofs/pkg/metrics/prometheus"
)

var (
	foreground bool
	pidFile    string
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the DittoFS server",
	Long: `Start the DittoFS server with the specified configuration.

The server will run in the foreground by default. Use --config to specify
a custom configuration file, or it will use the default location at
$XDG_CONFIG_HOME/dittofs/config.yaml.

Examples:
  # Start with default config
  dittofs start

  # Start with custom config file
  dittofs start --config /etc/dittofs/config.yaml

  # Start with environment variable overrides
  DITTOFS_LOGGING_LEVEL=DEBUG dittofs start`,
	RunE: runStart,
}

func init() {
	startCmd.Flags().BoolVar(&foreground, "foreground", true, "Run in foreground")
	startCmd.Flags().StringVar(&pidFile, "pid-file", "", "Path to PID file")
}

func runStart(cmd *cobra.Command, args []string) error {
	configFile := GetConfigFile()

	// Check if config exists
	if configFile == "" {
		// Check default location
		if !config.DefaultConfigExists() {
			return fmt.Errorf("no configuration file found at default location: %s\n\n"+
				"Please initialize a configuration file first:\n"+
				"  dittofs init\n\n"+
				"Or specify a custom config file:\n"+
				"  dittofs start --config /path/to/config.yaml",
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

	// Create cancellable context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize OpenTelemetry (if enabled)
	telemetryCfg := telemetry.Config{
		Enabled:        cfg.Telemetry.Enabled,
		ServiceName:    "dittofs",
		ServiceVersion: Version,
		Endpoint:       cfg.Telemetry.Endpoint,
		Insecure:       cfg.Telemetry.Insecure,
		SampleRate:     cfg.Telemetry.SampleRate,
	}
	telemetryShutdown, err := telemetry.Init(ctx, telemetryCfg)
	if err != nil {
		return fmt.Errorf("failed to initialize telemetry: %w", err)
	}
	defer func() {
		if err := telemetryShutdown(ctx); err != nil {
			logger.Error("telemetry shutdown error", "error", err)
		}
	}()

	// Initialize Pyroscope profiling (if enabled)
	profilingCfg := telemetry.ProfilingConfig{
		Enabled:        cfg.Telemetry.Profiling.Enabled,
		ServiceName:    "dittofs",
		ServiceVersion: Version,
		Endpoint:       cfg.Telemetry.Profiling.Endpoint,
		ProfileTypes:   cfg.Telemetry.Profiling.ProfileTypes,
	}
	profilingShutdown, err := telemetry.InitProfiling(profilingCfg)
	if err != nil {
		return fmt.Errorf("failed to initialize profiling: %w", err)
	}
	defer func() {
		if err := profilingShutdown(); err != nil {
			logger.Error("profiling shutdown error", "error", err)
		}
	}()

	fmt.Println("DittoFS - A modular virtual filesystem")
	logger.Info("Log level", "level", cfg.Logging.Level, "format", cfg.Logging.Format)
	logger.Info("Configuration loaded", "source", getConfigSource(configFile))
	if telemetry.IsEnabled() {
		logger.Info("Telemetry enabled", "endpoint", cfg.Telemetry.Endpoint, "sample_rate", cfg.Telemetry.SampleRate)
	} else {
		logger.Info("Telemetry disabled")
	}
	if telemetry.IsProfilingEnabled() {
		logger.Info("Profiling enabled", "endpoint", cfg.Telemetry.Profiling.Endpoint, "profile_types", cfg.Telemetry.Profiling.ProfileTypes)
	} else {
		logger.Info("Profiling disabled")
	}

	// Initialize metrics FIRST (before creating stores that use metrics)
	metricsResult := config.InitializeMetrics(cfg)

	// Initialize registry with all stores and shares
	reg, err := config.InitializeRegistry(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize registry: %w", err)
	}
	logger.Info("Registry initialized",
		"metadata_stores", reg.CountMetadataStores(),
		"shares", reg.CountShares())

	// Log share details
	for _, shareName := range reg.ListShares() {
		share, _ := reg.GetShare(shareName)
		logger.Info("Share configured",
			"name", share.Name,
			"metadata_store", share.MetadataStore,
			"read_only", share.ReadOnly)
	}

	// Create DittoServer with registry and shutdown timeout
	dittoSrv := dittoServer.New(reg, cfg.Server.ShutdownTimeout)

	if metricsResult.Server != nil {
		logger.Info("Metrics enabled", "port", cfg.Server.Metrics.Port)
		dittoSrv.SetMetricsServer(metricsResult.Server)
	} else {
		logger.Info("Metrics collection disabled")
	}

	// Initialize API server (if enabled - defaults to true)
	if cfg.Server.API.IsEnabled() {
		// Initialize control plane store for user management
		cpStore, err := store.New(&cfg.Database)
		if err != nil {
			return fmt.Errorf("failed to initialize control plane store: %w", err)
		}

		// Ensure admin user exists (generates random password on first run)
		adminPassword, err := cpStore.EnsureAdminUser(ctx)
		if err != nil {
			return fmt.Errorf("failed to ensure admin user: %w", err)
		}
		if adminPassword != "" {
			logger.Info("Admin user created", "username", "admin", "password", adminPassword)
			fmt.Printf("\n*** IMPORTANT: Admin user created with password: %s ***\n", adminPassword)
			fmt.Println("Please save this password. It will not be shown again.")
			fmt.Println()
		}

		// Set identity store on registry for protocol handlers
		// GORMStore directly implements models.IdentityStore
		reg.SetIdentityStore(cpStore)

		// Create API server (JWT service is created internally from config)
		// Pass nil for runtime - full runtime integration is a future step
		apiServer, err := api.NewServer(cfg.Server.API, nil, cpStore)
		if err != nil {
			return fmt.Errorf("failed to create API server: %w", err)
		}
		dittoSrv.SetAPIServer(apiServer)
		logger.Info("API server enabled", "port", cfg.Server.API.Port)
	} else {
		logger.Info("API server disabled")
	}

	// Create all enabled adapters using the factory
	adapters, err := config.CreateAdapters(cfg, metricsResult.NFSMetrics)
	if err != nil {
		return fmt.Errorf("failed to create adapters: %w", err)
	}

	// Add all adapters to the server
	for _, adapter := range adapters {
		if err := dittoSrv.AddAdapter(adapter); err != nil {
			return fmt.Errorf("failed to add %s adapter: %w", adapter.Protocol(), err)
		}
		logger.Info("Adapter enabled", "protocol", adapter.Protocol(), "port", adapter.Port())
	}

	// Write PID file if specified
	if pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
			return fmt.Errorf("failed to write PID file: %w", err)
		}
		defer func() { _ = os.Remove(pidFile) }()
	}

	// Start server in background
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- dittoSrv.Serve(ctx)
	}()

	// Wait for interrupt signal or server error
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("Server is running. Press Ctrl+C to stop.")

	select {
	case <-sigChan:
		signal.Stop(sigChan)
		logger.Info("Shutdown signal received, initiating graceful shutdown")
		cancel()

		// Wait for server to shut down gracefully
		if err := <-serverDone; err != nil {
			logger.Error("Server shutdown error", "error", err)
			return err
		}
		logger.Info("Server stopped gracefully")

	case err := <-serverDone:
		signal.Stop(sigChan)
		if err != nil {
			logger.Error("Server error", "error", err)
			return err
		}
		logger.Info("Server stopped")
	}

	return nil
}

// getConfigSource returns a description of where the config was loaded from.
func getConfigSource(configFile string) string {
	if configFile != "" {
		return configFile
	}
	if config.DefaultConfigExists() {
		return config.GetDefaultConfigPath()
	}
	return "defaults"
}
