package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/marmos91/dittofs/cmd/dittofs/commands"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/api"
	"github.com/marmos91/dittofs/pkg/config"
	dittoServer "github.com/marmos91/dittofs/pkg/server"

	// Import prometheus metrics to register init() functions
	_ "github.com/marmos91/dittofs/pkg/metrics/prometheus"
)

// Build-time variables injected via ldflags
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `DittoFS - Modular virtual filesystem

Usage:
  dittofs <command> [flags]

Commands:
  init     Initialize a sample configuration file
  start    Start the DittoFS server
  version  Show version information
  user     Manage users (add, delete, list, passwd, grant, revoke, groups, join, leave)
  group    Manage groups (add, delete, list, members, grant, revoke)

Flags:
  --config string    Path to config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
  --force            Force overwrite existing config file (init command only)

Examples:
  # Initialize config file
  dittofs init

  # Start server with default config location
  dittofs start

  # Start server with custom config
  dittofs start --config /etc/dittofs/config.yaml

  # User management
  dittofs user add alice
  dittofs user grant alice /export read-write
  dittofs user list

  # Group management
  dittofs group add editors
  dittofs group grant editors /export read-write
  dittofs group list

  # Use environment variables to override config
  DITTOFS_LOGGING_LEVEL=DEBUG dittofs start

Environment Variables:
  All configuration options can be overridden using environment variables.
  Format: DITTOFS_<SECTION>_<KEY> (use underscores for nested keys)

  Examples:
    DITTOFS_LOGGING_LEVEL=DEBUG
    DITTOFS_ADAPTERS_NFS_PORT=3049
    DITTOFS_CONTENT_FILESYSTEM_PATH=/custom/path

For more information, visit: https://github.com/marmos91/dittofs
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "init":
		runInit()
	case "start":
		runStart()
	case "user":
		runUser()
	case "group":
		runGroup()
	case "help", "--help", "-h":
		fmt.Print(usage)
		os.Exit(0)
	case "version", "--version", "-v":
		fmt.Printf("dittofs %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

// runInit handles the init subcommand
func runInit() {
	// Parse flags for init command
	initFlags := flag.NewFlagSet("init", flag.ExitOnError)
	configFile := initFlags.String("config", "", "Path to config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)")
	force := initFlags.Bool("force", false, "Force overwrite existing config file")

	if err := initFlags.Parse(os.Args[2:]); err != nil {
		log.Fatalf("Failed to parse flags: %v", err)
	}

	var configPath string
	var err error

	if *configFile != "" {
		// Use custom path
		err = config.InitConfigToPath(*configFile, *force)
		configPath = *configFile
	} else {
		// Use default path
		configPath, err = config.InitConfig(*force)
	}

	if err != nil {
		log.Fatalf("Failed to initialize config: %v", err)
	}

	fmt.Printf("✓ Configuration file created at: %s\n", configPath)
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Edit the configuration file to customize your setup")
	fmt.Println("  2. Start the server with: dittofs start")
	fmt.Printf("  3. Or specify custom config: dittofs start --config %s\n", configPath)
}

// runStart handles the start subcommand
func runStart() {
	// Parse flags for start command
	startFlags := flag.NewFlagSet("start", flag.ExitOnError)
	configFile := startFlags.String("config", "", "Path to config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)")

	if err := startFlags.Parse(os.Args[2:]); err != nil {
		log.Fatalf("Failed to parse flags: %v", err)
	}

	// Check if config exists
	if *configFile == "" {
		// Check default location
		if !config.DefaultConfigExists() {
			fmt.Fprintf(os.Stderr, "Error: No configuration file found at default location: %s\n\n", config.GetDefaultConfigPath())
			fmt.Fprintln(os.Stderr, "Please initialize a configuration file first:")
			fmt.Fprintln(os.Stderr, "  dittofs init")
			fmt.Fprintln(os.Stderr, "\nOr specify a custom config file:")
			fmt.Fprintln(os.Stderr, "  dittofs start --config /path/to/config.yaml")
			os.Exit(1)
		}
	} else {
		// Check explicitly specified path
		if _, err := os.Stat(*configFile); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: Configuration file not found: %s\n\n", *configFile)
			fmt.Fprintln(os.Stderr, "Please create the configuration file:")
			fmt.Fprintf(os.Stderr, "  dittofs init --config %s\n", *configFile)
			os.Exit(1)
		}
	}

	// Load configuration
	cfg, err := config.Load(*configFile)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize the structured logger
	loggerCfg := logger.Config{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
		Output: cfg.Logging.Output,
	}
	if err := logger.Init(loggerCfg); err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}

	// Create cancellable context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize OpenTelemetry (if enabled)
	telemetryCfg := telemetry.Config{
		Enabled:        cfg.Telemetry.Enabled,
		ServiceName:    "dittofs",
		ServiceVersion: version,
		Endpoint:       cfg.Telemetry.Endpoint,
		Insecure:       cfg.Telemetry.Insecure,
		SampleRate:     cfg.Telemetry.SampleRate,
	}
	telemetryShutdown, err := telemetry.Init(ctx, telemetryCfg)
	if err != nil {
		log.Fatalf("Failed to initialize telemetry: %v", err)
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
		ServiceVersion: version,
		Endpoint:       cfg.Telemetry.Profiling.Endpoint,
		ProfileTypes:   cfg.Telemetry.Profiling.ProfileTypes,
	}
	profilingShutdown, err := telemetry.InitProfiling(profilingCfg)
	if err != nil {
		log.Fatalf("Failed to initialize profiling: %v", err)
	}
	defer func() {
		if err := profilingShutdown(); err != nil {
			logger.Error("profiling shutdown error", "error", err)
		}
	}()

	fmt.Println("DittoFS - A modular virtual filesystem")
	logger.Info("Log level", "level", cfg.Logging.Level, "format", cfg.Logging.Format)
	logger.Info("Configuration loaded", "source", getConfigSource(*configFile))
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
	// This ensures metrics.IsEnabled() returns true when stores are created
	metricsResult := config.InitializeMetrics(cfg)

	// Initialize registry with all stores and shares
	reg, err := config.InitializeRegistry(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to initialize registry: %v", err)
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
		apiServer := api.NewServer(cfg.Server.API, reg)
		dittoSrv.SetAPIServer(apiServer)
		logger.Info("API server enabled", "port", cfg.Server.API.Port)
	} else {
		logger.Info("API server disabled")
	}

	// Create all enabled adapters using the factory
	adapters, err := config.CreateAdapters(cfg, metricsResult.NFSMetrics)
	if err != nil {
		log.Fatalf("Failed to create adapters: %v", err)
	}

	// Add all adapters to the server
	for _, adapter := range adapters {
		if err := dittoSrv.AddAdapter(adapter); err != nil {
			log.Fatalf("Failed to add %s adapter: %v", adapter.Protocol(), err)
		}
		logger.Info("Adapter enabled", "protocol", adapter.Protocol(), "port", adapter.Port())
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
		signal.Stop(sigChan) // Stop signal notification immediately after receiving signal
		logger.Info("Shutdown signal received, initiating graceful shutdown")
		cancel() // Cancel context to initiate shutdown

		// Wait for server to shut down gracefully
		if err := <-serverDone; err != nil {
			logger.Error("Server shutdown error", "error", err)
			os.Exit(1)
		}
		logger.Info("Server stopped gracefully")

	case err := <-serverDone:
		signal.Stop(sigChan) // Stop signal notification when server stops
		if err != nil {
			logger.Error("Server error", "error", err)
			os.Exit(1)
		}
		logger.Info("Server stopped")
	}
}

// runUser handles the user subcommand
func runUser() {
	cmd := commands.NewUserCommand()
	if err := cmd.Run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// runGroup handles the group subcommand
func runGroup() {
	cmd := commands.NewGroupCommand()
	if err := cmd.Run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// getConfigSource returns a description of where the config was loaded from
func getConfigSource(configFile string) string {
	if configFile != "" {
		return configFile
	}
	if config.DefaultConfigExists() {
		return config.GetDefaultConfigPath()
	}
	return "defaults"
}
