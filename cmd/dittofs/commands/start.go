package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	rt.SetAdapterFactory(createAdapterFactory())

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

// startDaemon starts the server as a background daemon process.
func startDaemon() error {
	// Determine state directory for PID and log files
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		stateDir = filepath.Join(homeDir, ".local", "state")
	}
	dittofsStateDir := filepath.Join(stateDir, "dittofs")

	// Create state directory if it doesn't exist
	if err := os.MkdirAll(dittofsStateDir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Set default PID file if not specified
	pidPath := pidFile
	if pidPath == "" {
		pidPath = filepath.Join(dittofsStateDir, "dittofs.pid")
	}

	// Check if already running
	if _, err := os.Stat(pidPath); err == nil {
		pidData, err := os.ReadFile(pidPath)
		if err == nil {
			var pid int
			if _, err := fmt.Sscanf(string(pidData), "%d", &pid); err == nil {
				// Check if process is still running
				if process, err := os.FindProcess(pid); err == nil {
					if err := process.Signal(syscall.Signal(0)); err == nil {
						return fmt.Errorf("DittoFS is already running (PID %d)\nUse 'dittofs stop' to stop the running instance", pid)
					}
				}
			}
		}
		// Stale PID file, remove it
		_ = os.Remove(pidPath)
	}

	// Set default log file if not specified
	logPath := logFile
	if logPath == "" {
		logPath = filepath.Join(dittofsStateDir, "dittofs.log")
	}

	// Get the executable path
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	// Build arguments for the daemon process
	daemonArgs := []string{"start", "--foreground", "--pid-file", pidPath}
	if GetConfigFile() != "" {
		daemonArgs = append(daemonArgs, "--config", GetConfigFile())
	}

	// Create the daemon process
	cmd := exec.Command(executable, daemonArgs...)

	// Open log file for stdout/stderr
	logFileHandle, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	cmd.Stdout = logFileHandle
	cmd.Stderr = logFileHandle

	// Detach from parent process
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	// Start the daemon
	if err := cmd.Start(); err != nil {
		_ = logFileHandle.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	_ = logFileHandle.Close()

	fmt.Printf("DittoFS started in background (PID %d)\n", cmd.Process.Pid)
	fmt.Printf("  PID file: %s\n", pidPath)
	fmt.Printf("  Log file: %s\n", logPath)
	fmt.Println("\nUse 'dittofs stop' to stop the server")
	fmt.Println("Use 'dittofs status' to check server status")

	return nil
}

// createAdapterFactory returns a factory function that creates protocol adapters
// from configuration. This factory is used by Runtime to create adapters
// dynamically when loading from store or when created via API.
func createAdapterFactory() runtime.AdapterFactory {
	return func(cfg *models.AdapterConfig) (runtime.ProtocolAdapter, error) {
		switch cfg.Type {
		case "nfs":
			nfsCfg := nfs.NFSConfig{
				Enabled: true,
				Port:    cfg.Port,
			}
			if nfsCfg.Port == 0 {
				nfsCfg.Port = 12049 // Default NFS port
			}
			return nfs.New(nfsCfg, nil), nil

		case "smb":
			smbCfg := smb.SMBConfig{
				Enabled: true,
				Port:    cfg.Port,
			}
			if smbCfg.Port == 0 {
				smbCfg.Port = 12445 // Default SMB port
			}
			return smb.New(smbCfg), nil

		default:
			return nil, fmt.Errorf("unknown adapter type: %s", cfg.Type)
		}
	}
}
