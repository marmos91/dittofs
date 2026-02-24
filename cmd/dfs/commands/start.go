package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/adapter/smb"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/spf13/cobra"

	// Import prometheus metrics to register init() functions
	_ "github.com/marmos91/dittofs/pkg/metrics/prometheus"
)

var (
	foreground bool
	pidFile    string
	logFile    string
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the DittoFS server",
	Long: `Start the DittoFS server with the specified configuration.

By default, the server runs in the background (daemon mode). Use --foreground
to run in the foreground for debugging or when managed by a process supervisor.

Use --config to specify a custom configuration file, or it will use the
default location at $XDG_CONFIG_HOME/dittofs/config.yaml.

Examples:
  # Start in background (default)
  dittofs start

  # Start in foreground
  dittofs start --foreground

  # Start with custom config file
  dittofs start --config /etc/dittofs/config.yaml

  # Start with environment variable overrides
  DITTOFS_LOGGING_LEVEL=DEBUG dittofs start --foreground`,
	RunE: runStart,
}

func init() {
	startCmd.Flags().BoolVarP(&foreground, "foreground", "f", false, "Run in foreground (default: background/daemon mode)")
	startCmd.Flags().StringVar(&pidFile, "pid-file", "", "Path to PID file (default: $XDG_STATE_HOME/dittofs/dittofs.pid)")
	startCmd.Flags().StringVar(&logFile, "log-file", "", "Path to log file for daemon mode (default: $XDG_STATE_HOME/dittofs/dittofs.log)")
}

func runStart(cmd *cobra.Command, args []string) error {
	// Handle daemon mode (background)
	if !foreground {
		return startDaemon()
	}

	cfg, err := config.MustLoad(GetConfigFile())
	if err != nil {
		return err
	}

	// Initialize the structured logger
	if err := InitLogger(cfg); err != nil {
		return err
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
	logger.Info("Configuration loaded", "source", getConfigSource(GetConfigFile()))
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

	// Initialize metrics (if enabled)
	metricsResult := config.InitializeMetrics(cfg)

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

	// Ensure default groups exist (admins, users) and add admin to admins group
	groupsCreated, err := cpStore.EnsureDefaultGroups(ctx)
	if err != nil {
		return fmt.Errorf("failed to ensure default groups: %w", err)
	}
	if groupsCreated {
		logger.Info("Default groups created", "groups", "admins, users")
	}

	// Ensure default adapters exist (NFS and SMB)
	adaptersCreated, err := cpStore.EnsureDefaultAdapters(ctx)
	if err != nil {
		return fmt.Errorf("failed to ensure default adapters: %w", err)
	}
	if adaptersCreated {
		logger.Info("Default adapters created", "adapters", "nfs, smb")
	}

	// Initialize runtime from database (loads metadata stores and shares)
	rt, err := runtime.InitializeFromStore(ctx, cpStore)
	if err != nil {
		return fmt.Errorf("failed to initialize runtime: %w", err)
	}

	// Store cache config BEFORE loading shares (AddShare needs it for PayloadService)
	rt.SetCacheConfig(&runtime.CacheConfig{
		Path: cfg.Cache.Path,
		Size: uint64(cfg.Cache.Size),
	})
	logger.Info("Cache configuration stored", "path", cfg.Cache.Path, "size", cfg.Cache.Size)

	// Now load shares (they need cache config to initialize PayloadService)
	if err := runtime.LoadSharesFromStore(ctx, rt, cpStore); err != nil {
		logger.Warn("Failed to load some shares", "error", err)
	}

	logger.Info("Runtime initialized",
		"metadata_stores", rt.CountMetadataStores(),
		"shares", rt.CountShares())

	// If payload stores already exist in DB, create the PayloadService now
	if err := rt.EnsurePayloadService(ctx); err != nil {
		// Don't fail if no payload stores configured - it will be created when first one is added
		logger.Info("PayloadService not initialized (will be created when first payload store is added)", "reason", err)
	}

	// Configure runtime
	rt.SetShutdownTimeout(cfg.ShutdownTimeout)
	rt.SetAdapterFactory(createAdapterFactory(&cfg.Kerberos))

	// Set metrics server if enabled
	if metricsResult.Server != nil {
		logger.Info("Metrics enabled", "port", cfg.Metrics.Port)
		rt.SetMetricsServer(metricsResult.Server)
	} else {
		logger.Info("Metrics collection disabled")
	}

	// Create and set API server
	apiServer, err := api.NewServer(cfg.ControlPlane, rt, cpStore)
	if err != nil {
		return fmt.Errorf("failed to create API server: %w", err)
	}
	rt.SetAPIServer(apiServer)
	logger.Info("API server configured", "port", cfg.ControlPlane.Port)

	// Write PID file if specified
	if pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
			return fmt.Errorf("failed to write PID file: %w", err)
		}
		defer func() { _ = os.Remove(pidFile) }()
	}

	// Start runtime in background (loads adapters from store automatically)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- rt.Serve(ctx)
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

// createAdapterFactory returns a factory function that creates protocol adapters
// from configuration. This factory is used by Runtime to create adapters
// dynamically when loading from store or when created via API.
func createAdapterFactory(kerberosConfig *config.KerberosConfig) runtime.AdapterFactory {
	return func(cfg *models.AdapterConfig) (runtime.ProtocolAdapter, error) {
		// Parse the JSON config if present
		parsedConfig, err := cfg.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to parse adapter config: %w", err)
		}

		switch cfg.Type {
		case "nfs":
			nfsCfg := nfs.NFSConfig{
				Enabled: true,
				Port:    cfg.Port,
			}
			if nfsCfg.Port == 0 {
				nfsCfg.Port = 12049 // Default NFS port
			}
			adapter := nfs.New(nfsCfg, nil)
			// Configure Kerberos if enabled
			if kerberosConfig != nil && kerberosConfig.Enabled {
				adapter.SetKerberosConfig(kerberosConfig)
			}
			return adapter, nil

		case "smb":
			smbCfg := smb.SMBConfig{
				Enabled: true,
				Port:    cfg.Port,
			}
			if smbCfg.Port == 0 {
				smbCfg.Port = 12445 // Default SMB port
			}

			// Apply parsed config for SMB-specific settings
			if parsedConfig != nil {
				if signingCfg, ok := parsedConfig["signing"].(map[string]any); ok {
					if enabled, ok := signingCfg["enabled"].(bool); ok {
						smbCfg.Signing.Enabled = &enabled
					}
					if required, ok := signingCfg["required"].(bool); ok {
						smbCfg.Signing.Required = required
					}
				}
			}

			return smb.New(smbCfg), nil

		default:
			return nil, fmt.Errorf("unknown adapter type: %s", cfg.Type)
		}
	}
}
