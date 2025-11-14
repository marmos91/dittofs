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
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/config"
	dittoServer "github.com/marmos91/dittofs/pkg/server"
)

const usage = `DittoFS - Dynamic NFS Server

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
	if *configFile == "" && !config.ConfigExists() {
		fmt.Fprintf(os.Stderr, "Error: No configuration file found at default location: %s\n\n", config.GetDefaultConfigPath())
		fmt.Fprintln(os.Stderr, "Please initialize a configuration file first:")
		fmt.Fprintln(os.Stderr, "  dittofs init")
		fmt.Fprintln(os.Stderr, "\nOr specify a custom config file:")
		fmt.Fprintln(os.Stderr, "  dittofs start --config /path/to/config.yaml")
		os.Exit(1)
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

	fmt.Println("DittoFS - Dynamic NFS Server")
	logger.Info("Log level: %s", cfg.Logging.Level)
	logger.Info("Configuration loaded from: %s", getConfigSource(*configFile))

	// Create content store
	contentStore, err := config.CreateContentStore(ctx, &cfg.Content)
	if err != nil {
		log.Fatalf("Failed to create content store: %v", err)
	}
	logger.Info("Content store initialized: type=%s", cfg.Content.Type)

	// Create metadata store
	metadataStore, err := config.CreateMetadataStore(ctx, &cfg.Metadata)
	if err != nil {
		log.Fatalf("Failed to create metadata store: %v", err)
	}
	logger.Info("Metadata store initialized: type=%s", cfg.Metadata.Type)

	// Configure metadata store settings
	if err := config.ConfigureMetadataStore(ctx, metadataStore, &cfg.Metadata); err != nil {
		log.Fatalf("Failed to configure metadata store: %v", err)
	}

	// Create shares
	if err := config.CreateShares(ctx, metadataStore, cfg.Shares); err != nil {
		log.Fatalf("Failed to create shares: %v", err)
	}
	logger.Info("Configured %d share(s)", len(cfg.Shares))
	for _, share := range cfg.Shares {
		logger.Info("  - %s (read_only=%v)", share.Name, share.ReadOnly)
	}

	// Create DittoServer
	dittoSrv := dittoServer.New(metadataStore, contentStore)

	// Add enabled adapters
	if cfg.Adapters.NFS.Enabled {
		// Use the config's NFSConfig directly (no manual mapping needed)
		nfsAdapter := nfs.New(cfg.Adapters.NFS)
		if err := dittoSrv.AddAdapter(nfsAdapter); err != nil {
			log.Fatalf("Failed to add NFS adapter: %v", err)
		}
		logger.Info("NFS adapter enabled on port %d", nfsAdapter.Port())
	}

	// Start server in background
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- dittoSrv.Serve(ctx)
	}()

	// Wait for interrupt signal or server error
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan) // Clean up signal notification to prevent goroutine leak

	logger.Info("Server is running. Press Ctrl+C to stop.")

	select {
	case <-sigChan:
		logger.Info("Shutdown signal received, initiating graceful shutdown...")
		cancel() // Cancel context to initiate shutdown

		// Wait for server to shut down gracefully
		if err := <-serverDone; err != nil {
			logger.Error("Server shutdown error: %v", err)
			os.Exit(1)
		}
		logger.Info("Server stopped gracefully")

	case err := <-serverDone:
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
	if config.ConfigExists() {
		return config.GetDefaultConfigPath()
	}
	return "defaults"
}
