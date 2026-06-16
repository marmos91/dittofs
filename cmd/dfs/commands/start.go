package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/sysinfo"
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/adapter/smb"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metrics"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// isTerminal reports whether the given file descriptor is an interactive
// terminal. Indirected through a package-level var so tests can drive both
// branches of emitAdminPassword deterministically.
var isTerminal = func(fd uintptr) bool {
	return term.IsTerminal(int(fd))
}

// EX_CONFIG is the exit code per sysexits(3) — "configuration error".
// Used by the legacy-layout boot guard when a share directory still
// contains pre-v0.16 `.blk` files without a `.cas-migrated-v1` sentinel.
// The operator runs `dfs migrate-to-cas` before retrying.
const EX_CONFIG = 78

// exitFn is the production exit path for the legacy-layout boot guard.
// Indirected through a package-level var so the in-process boot-guard
// test (start_test.go::TestStart_LegacyLayoutExitCode) can stub it to
// capture the exit code deterministically without spawning a subprocess.
// Production code MUST NOT reassign exitFn — only the test does, and
// only via a t.Cleanup-restored override.
var exitFn = os.Exit

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
  dfs start

  # Start in foreground
  dfs start --foreground

  # Start with custom config file
  dfs start --config /etc/dittofs/config.yaml

  # Start with environment variable overrides
  DITTOFS_LOGGING_LEVEL=DEBUG dfs start --foreground`,
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

	fmt.Println("DittoFS - A modular virtual filesystem")
	logger.Info("Log level", "level", cfg.Logging.Level, "format", cfg.Logging.Format)
	logger.Info("Configuration loaded", "source", getConfigSource(GetConfigFile()))

	// Initialize control plane store for user management
	cpStore, err := store.New(&cfg.Database)
	if err != nil {
		return fmt.Errorf("failed to initialize control plane store: %w", err)
	}

	// Ensure admin user exists (generates random password on first run).
	// Whether the first-login password change is forced is operator-configurable
	// (controlplane.require_initial_password_change, default true).
	adminPassword, err := cpStore.EnsureAdminUser(ctx, cfg.ControlPlane.RequiresInitialPasswordChange())
	if err != nil {
		return fmt.Errorf("failed to ensure admin user: %w", err)
	}
	if adminPassword != "" {
		logger.Info("Admin user created", "username", "admin")
		emitAdminPassword(adminPassword)
	}

	// Ensure default groups exist (admins, operators, users) and add admin to admins group
	groupsCreated, err := cpStore.EnsureDefaultGroups(ctx)
	if err != nil {
		return fmt.Errorf("failed to ensure default groups: %w", err)
	}
	if groupsCreated {
		logger.Info("Default groups created", "groups", "admins, operators, users")
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

	// Configure the background snapshot scheduler before Serve launches it.
	rt.SetSnapshotSchedulerConfig(cfg.Snapshot.SchedulerPollInterval, cfg.Snapshot.SchedulerDisabled)

	// Auto-deduce block store defaults from system resources
	detector := sysinfo.NewDetector()
	deduced := block.DeduceDefaults(detector)

	logger.Info("System resources detected",
		"memory", block.FormatBytes(detector.AvailableMemory()),
		"memory_source", detector.MemorySource(),
		"cpus", detector.AvailableCPUs(),
	)
	logger.Info("Auto-deduced block store defaults",
		"local_store_size", block.FormatBytes(deduced.LocalStoreSize),
		"read_buffer_size", block.FormatBytes(uint64(deduced.ReadBufferSize)),
		"max_pending_size", block.FormatBytes(deduced.MaxPendingSize),
		"parallel_syncs", deduced.ParallelSyncs,
		"parallel_fetches", deduced.ParallelFetches,
		"prefetch_workers", deduced.PrefetchWorkers,
	)

	if floors := deduced.HitFloors(); len(floors) > 0 {
		logger.Warn("Some deduced values hit minimum floors; system may be resource-constrained",
			"floors", floors,
			"system_memory", block.FormatBytes(detector.AvailableMemory()),
			"system_cpus", detector.AvailableCPUs(),
		)
	}

	// Set per-share defaults BEFORE loading shares (AddShare creates BlockStores).
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{
		MaxSize:                deduced.LocalStoreSize,
		MaxMemory:              block.ClampToInt64(deduced.MaxPendingSize),
		ReadBufferBytes:        deduced.ReadBufferSize,
		DedupLRUSize:           cfg.Blockstore.Local.DedupLRUSize,
		DefaultRemoteCacheSize: cfg.Blockstore.Local.DefaultRemoteCacheSize,
		BackpressureMaxWait:    cfg.Blockstore.Local.BackpressureMaxWait,
	})
	rt.SetSyncerDefaults(&shares.SyncerDefaults{
		ParallelUploads:   deduced.ParallelSyncs,
		ParallelDownloads: deduced.ParallelFetches,
		PrefetchWorkers:   deduced.PrefetchWorkers,
	})
	// Wire operator-configured GC knobs into the runtime so
	// engine.CollectGarbage receives them in engine.Options. Without this,
	// the validated gc.* config in pkg/config/config.go is silently dropped
	// and the engine falls back to hardcoded defaults.
	rt.SetGCDefaults(&runtime.GCDefaults{
		GracePeriod:      cfg.GC.GracePeriod,
		DryRunSampleSize: cfg.GC.DryRunSampleSize,
	})

	// Thread the operator-configured lock-manager grace period into the
	// MetadataService BEFORE loading shares: AddShare registers each share's
	// lock manager and enters the post-restart grace window, so the duration
	// must be set first. LoadInitial is idempotent (the lifecycle Serve loop
	// loads settings again); a nil/zero value falls back to the 90s default,
	// which matches the DB default for grace_period.
	if sw := rt.GetSettingsWatcher(); sw != nil {
		if err := sw.LoadInitial(ctx); err != nil {
			logger.Warn("Failed to load initial adapter settings for lock grace period", "error", err)
		}
		if nfsSettings := rt.GetNFSSettings(); nfsSettings != nil && nfsSettings.GracePeriod > 0 {
			rt.GetMetadataService().SetLockGracePeriod(
				time.Duration(nfsSettings.GracePeriod) * time.Second)
		}
	}

	// Load shares (per-share BlockStores are created during AddShare).
	// Legacy-layout detection is a hard boot stop. Other share-loading
	// failures stay best-effort (logged + ignored, the historical
	// behavior).
	if stop := handleLoadSharesError(runtime.LoadSharesFromStore(ctx, rt, cpStore), os.Stderr); stop {
		return nil
	}

	logger.Info("Runtime initialized",
		"metadata_stores", rt.CountMetadataStores(),
		"shares", rt.CountShares())
	logger.Info("Per-share BlockStores created during share loading")

	// Configure runtime
	rt.SetShutdownTimeout(cfg.ShutdownTimeout)
	rt.SetAdapterFactory(createAdapterFactory(&cfg.Kerberos))

	// Create and set API server
	apiServer, err := api.NewServer(cfg.ControlPlane, rt, cpStore, cfg.Snapshot.RestoreHTTPTimeout)
	if err != nil {
		return fmt.Errorf("failed to create API server: %w", err)
	}
	rt.SetAPIServer(apiServer)
	logger.Info("API server configured",
		"host", cfg.ControlPlane.Host,
		"port", cfg.ControlPlane.Port,
		"tls", apiServer.TLSEnabled())

	// Build the metrics registry unconditionally so inline instruments (adapter
	// RED, connection, auth counters) always record; only the /metrics listener
	// is gated by config. The runtime carries the handle to every adapter.
	m := metrics.New(Version, Commit)
	m.RegisterProvider(rt)
	rt.SetMetrics(m)

	// Configure the Prometheus metrics endpoint (opt-in). It runs on its own
	// listener, separate from the API server, so scrapers reach it without API
	// auth and the two surfaces have independent lifecycles.
	metricsServer, err := buildMetricsListener(&cfg.Metrics, m)
	if err != nil {
		return err
	}
	if metricsServer != nil {
		logger.Info("metrics endpoint configured",
			"addr", cfg.Metrics.Addr(), "path", cfg.Metrics.Path, "auth", cfg.Metrics.Auth)
	}

	// Write PID file if specified
	if pidFile != "" {
		if err := os.WriteFile(pidFile, fmt.Appendf(nil, "%d", os.Getpid()), 0644); err != nil {
			return fmt.Errorf("failed to write PID file: %w", err)
		}
		defer func() { _ = os.Remove(pidFile) }()
	}

	// Start runtime in background (loads adapters from store automatically)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- rt.Serve(ctx)
	}()

	// Start the metrics listener alongside, cancelled by the same context. A
	// metrics failure is logged but does not bring down the data plane.
	if metricsServer != nil {
		go func() {
			if err := metricsServer.Serve(ctx); err != nil {
				logger.Error("metrics server error", "error", err)
			}
		}()
	}

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

// buildMetricsListener constructs the Prometheus metrics HTTP listener for the
// given registry when the endpoint is enabled. Returns (nil, nil) when metrics
// serving is disabled (the registry still exists and records inline metrics).
func buildMetricsListener(cfg *config.MetricsConfig, m *metrics.Metrics) (*metrics.Server, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	var token string
	if cfg.Auth == "token" {
		b, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read metrics token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return nil, fmt.Errorf("metrics token file %q is empty", cfg.TokenFile)
		}
	}

	srv, err := metrics.NewServer(m, metrics.ServerOptions{
		Addr:       cfg.Addr(),
		Path:       cfg.Path,
		Token:      token,
		CertFile:   cfg.TLS.CertFile,
		KeyFile:    cfg.TLS.KeyFile,
		ClientCA:   cfg.TLS.ClientCA,
		MinVersion: cfg.TLS.MinVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("create metrics server: %w", err)
	}
	return srv, nil
}

// createAdapterFactory returns a factory function that creates protocol adapters
// from configuration. This factory is used by Runtime to create adapters
// dynamically when loading from store or when created via API.
func createAdapterFactory(kerberosConfig *config.KerberosConfig) runtime.AdapterFactory {
	return func(cfg *models.AdapterConfig) (runtime.ProtocolAdapter, error) {
		switch cfg.Type {
		case "nfs":
			return createNFSAdapter(cfg, kerberosConfig)
		case "smb":
			return createSMBAdapter(cfg, kerberosConfig)
		default:
			return nil, fmt.Errorf("unknown adapter type: %s", cfg.Type)
		}
	}
}

func createNFSAdapter(cfg *models.AdapterConfig, kerberosConfig *config.KerberosConfig) (runtime.ProtocolAdapter, error) {
	port := cfg.Port
	if port == 0 {
		port = 12049
	}

	adapter := nfs.New(nfs.NFSConfig{Enabled: true, Port: port})
	if kerberosConfig != nil && kerberosConfig.Enabled {
		adapter.SetKerberosConfig(kerberosConfig)
	}
	return adapter, nil
}

func createSMBAdapter(cfg *models.AdapterConfig, kerberosConfig *config.KerberosConfig) (runtime.ProtocolAdapter, error) {
	port := cfg.Port
	if port == 0 {
		port = 12445
	}

	smbCfg := smb.Config{Enabled: true, Port: port}

	parsedConfig, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to parse adapter config: %w", err)
	}

	if parsedConfig != nil {
		if bindAddr, ok := parsedConfig["bind_address"].(string); ok {
			smbCfg.BindAddress = bindAddr
		}
		if signingCfg, ok := parsedConfig["signing"].(map[string]any); ok {
			if enabled, ok := signingCfg["enabled"].(bool); ok {
				smbCfg.Signing.Enabled = &enabled
			}
			if required, ok := signingCfg["required"].(bool); ok {
				smbCfg.Signing.Required = required
			}
		}
	}

	smbAdapter := smb.New(smbCfg)

	// Wire Kerberos provider for SPNEGO authentication. When Kerberos is not
	// configured, the SMB adapter only accepts NTLM/guest auth.
	if kerberosConfig != nil && kerberosConfig.Enabled {
		provider, err := kerberos.NewProvider(kerberosConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize SMB Kerberos provider: %w", err)
		}
		smbAdapter.SetKerberosProvider(provider)
	}

	return smbAdapter, nil
}

// handleLoadSharesError centralizes the share-loading error policy so
// the in-process boot-guard test can exercise the exit-78 path without
// rebuilding the full daemon setup. Returns true when the caller should
// stop runStart (legacy-layout branch hit; exitFn called); false
// otherwise (no error, or a non-legacy warn-and-continue).
//
// Production code MUST go through this helper — direct termination
// from runStart would bypass the exitFn indirection the test depends
// on.
func handleLoadSharesError(err error, stderr *os.File) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, block.ErrLegacyLayoutDetected) {
		_, _ = fmt.Fprintln(stderr, formatLegacyLayoutDirective(err))
		exitFn(EX_CONFIG)
		// Unreachable in production (exitFn == os.Exit terminates).
		// Defensive: in-process tests stub exitFn to NOT terminate.
		return true
	}
	logger.Warn("Failed to load some shares", "error", err)
	return false
}

// emitAdminPassword surfaces a freshly-generated first-run admin password.
//
// It prints the plaintext password ONLY when stdout is an interactive terminal
// (an operator watching a foreground `dfs start`). In daemon mode the child's
// stdout is redirected to a persistent log file; writing the password there
// leaves a long-lived on-disk copy of an admin credential readable by anyone
// who can read the log. When stdout is not a terminal we instead emit a
// secret-free notice directing the operator to reset the password via dfsctl.
func emitAdminPassword(password string) {
	if isTerminal(os.Stdout.Fd()) {
		fmt.Printf("\n*** IMPORTANT: Admin user created with password: %s ***\n", password)
		fmt.Println("Please save this password. It will not be shown again.")
		fmt.Println()
		return
	}
	// Daemon / non-interactive: never write the secret to the log.
	logger.Warn("Admin user created with a generated password; " +
		"reset it with 'dfsctl user passwd admin' (the password is not written to the log)")
}

// formatLegacyLayoutDirective renders the multi-line operator directive
// printed when LoadSharesFromStore surfaces ErrLegacyLayoutDetected.
// The full wrapped error message (`share "<name>": share <path>:
// blockstore: legacy .blk layout detected (run `dfs migrate-to-cas`)`)
// is embedded verbatim so the operator sees BOTH the share name AND
// the offending path without fragile substring extraction.
func formatLegacyLayoutDirective(err error) string {
	return fmt.Sprintf(`Detected legacy .blk layout: %s.
v0.16+ requires CAS migration. Run:
    dfs migrate-to-cas --share <name>
or, to migrate every share at once:
    dfs migrate-to-cas
See docs/CONFIGURATION.md §migration.`, err)
}
