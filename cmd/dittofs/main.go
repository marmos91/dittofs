package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/config"
	dittoServer "github.com/marmos91/dittofs/pkg/server"
)

const usage = `DittoFS - Modular virtual filesystem

Usage:
  dittofs <command> [flags]

Commands:
  init     Initialize a sample configuration file
  start    Start the DittoFS server

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
	case "help", "--help", "-h":
		fmt.Print(usage)
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

	// Configure logger
	logger.SetLevel(cfg.Logging.Level)

	// Create cancellable context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("DittoFS - A modular virtual filesystem")
	logger.Info("Log level: %s", cfg.Logging.Level)
	logger.Info("Configuration loaded from: %s", getConfigSource(*configFile))

	// Initialize registry with all stores and shares
	reg, err := config.InitializeRegistry(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to initialize registry: %v", err)
	}
	logger.Info("Registry initialized: %d metadata store(s), %d content store(s), %d share(s)",
		reg.CountMetadataStores(), reg.CountContentStores(), reg.CountShares())

	// Log share details
	for _, shareName := range reg.ListShares() {
		share, _ := reg.GetShare(shareName)
		logger.Info("  - %s (metadata: %s, content: %s, read_only: %v)",
			share.Name, share.MetadataStore, share.ContentStore, share.ReadOnly)
	}

	// Create DittoServer with registry and shutdown timeout
	dittoSrv := dittoServer.New(reg, cfg.Server.ShutdownTimeout)

	// TODO(Phase 4): Update GC to work with Registry and multiple stores
	// The garbage collector currently expects single stores but we now have
	// a registry with potentially multiple stores. This needs to be refactored
	// to either:
	// 1. Create one GC per store pair, or
	// 2. Update GC to work with Registry directly
	//
	// For now, GC is disabled during the store-per-share refactor.
	if cfg.GC.Enabled {
		logger.Warn("Garbage collection is temporarily disabled during store-per-share refactor")
		logger.Warn("GC will be re-enabled in a future phase with multi-store support")
	}

	// Create all enabled adapters using the factory
	adapters, err := config.CreateAdapters(cfg, nil) // nil = no metrics for now
	if err != nil {
		log.Fatalf("Failed to create adapters: %v", err)
	}

	// Add all adapters to the server
	for _, adapter := range adapters {
		if err := dittoSrv.AddAdapter(adapter); err != nil {
			log.Fatalf("Failed to add %s adapter: %v", adapter.Protocol(), err)
		}
		logger.Info("%s adapter enabled on port %d", adapter.Protocol(), adapter.Port())
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
		logger.Info("Shutdown signal received, initiating graceful shutdown...")
		cancel() // Cancel context to initiate shutdown

		// Wait for server to shut down gracefully
		if err := <-serverDone; err != nil {
			logger.Error("Server shutdown error: %v", err)
			os.Exit(1)
		}
		logger.Info("Server stopped gracefully")

	case err := <-serverDone:
		signal.Stop(sigChan) // Stop signal notification when server stops
		if err != nil {
			logger.Error("Server error: %v", err)
			os.Exit(1)
		}
		logger.Info("Server stopped")
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
