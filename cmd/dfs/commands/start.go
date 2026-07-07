package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/marmos91/dittofs/internal/controlplane/api/handlers"
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
	"github.com/marmos91/dittofs/pkg/identity/ldap"
	badgerstore "github.com/marmos91/dittofs/pkg/metadata/store/badger"
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

By default, the server daemonizes into the background and writes its PID to
$XDG_STATE_HOME/dittofs/dittofs.pid. Use --foreground when running under a
process supervisor (systemd, Docker) or for interactive debugging. The NFS
adapter listens on port 12049 and the SMB adapter on port 12445 by default;
the control-plane REST API is available at http://localhost:8080.

Examples:
  # Start in background (daemon mode)
  dfs start

  # Start in foreground with debug logging
  DITTOFS_LOGGING_LEVEL=DEBUG dfs start --foreground

  # Start with a custom config file and explicit PID file path
  dfs start --config /etc/dittofs/config.yaml --pid-file /var/run/dittofs.pid

  # Set admin password via environment on first boot instead of the generated one
  DITTOFS_ADMIN_INITIAL_PASSWORD=changeme dfs start --foreground`,
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

	// Ensure admin user exists. On first run the password is taken from
	// admin.password_hash (config), DITTOFS_ADMIN_INITIAL_PASSWORD (env,
	// plaintext, also enables SMB admin), or a generated random password — in
	// that precedence. Whether the first-login password change is forced is
	// operator-configurable (controlplane.require_initial_password_change,
	// default true) and is skipped when the operator supplied the password.
	adminPassword, err := cpStore.EnsureAdminUser(ctx, cfg.ControlPlane.RequiresInitialPasswordChange(), cfg.Admin.PasswordHash)
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

	// Wire operator-configured Badger metadata-cache defaults into the badger
	// engine BEFORE any metadata store is opened (InitializeFromStore opens
	// them). Zero values leave that dimension to RAM-relative auto-sizing
	// inside the engine (#1245 Bug D).
	badgerstore.SetGlobalBadgerCacheDefaults(
		cfg.Metadata.Badger.BlockCacheSizeMB,
		cfg.Metadata.Badger.IndexCacheSizeMB,
	)

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
		"max_log_bytes", block.FormatBytes(deduced.MaxLogBytes),
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

	// Resolve the effective append-log pressure budget default: the global
	// config blockstore.local.max_log_bytes wins when set, otherwise the
	// system-deduced default. A per-share block store config max_log_bytes
	// still overrides this inside CreateLocalStoreFromConfig.
	effectiveMaxLogBytes := deduced.MaxLogBytes
	if cfg.Blockstore.Local.MaxLogBytes > 0 {
		effectiveMaxLogBytes = cfg.Blockstore.Local.MaxLogBytes
	}

	// Set per-share defaults BEFORE loading shares (AddShare creates BlockStores).
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{
		MaxSize:                deduced.LocalStoreSize,
		ReadBufferBytes:        deduced.ReadBufferSize,
		MaxLogBytes:            block.ClampToInt64(effectiveMaxLogBytes),
		DefaultRemoteCacheSize: cfg.Blockstore.Local.DefaultRemoteCacheSize,
		BackpressureMaxWait:    cfg.Blockstore.Local.BackpressureMaxWait,
	})
	rt.SetSyncerDefaults(&shares.SyncerDefaults{
		ParallelDownloads: deduced.ParallelFetches,
		PrefetchWorkers:   deduced.PrefetchWorkers,
	})
	// Wire operator-configured GC knobs into the runtime so
	// engine.CollectGarbage receives them in engine.Options. Without this,
	// the validated gc.* config in pkg/config/config.go is silently dropped
	// and the engine falls back to hardcoded defaults.
	rt.SetGCDefaults(&runtime.GCDefaults{
		GracePeriod:         cfg.GC.GracePeriod,
		DryRunSampleSize:    cfg.GC.DryRunSampleSize,
		CompactionLiveRatio: cfg.GC.CompactionLiveRatio,
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

	// One-time stranded-row reconcile migration (#1433): reaps file_blocks
	// rows leaked by the pre-fix delete path and sweeps the now-orphaned
	// chunks. Launched AFTER LoadSharesFromStore so ListShares() sees the
	// registered shares (otherwise it no-ops and never sets its marker).
	// Guarded by a per-store marker so it runs once per store. Detached from
	// ctx (WithoutCancel) so a shutdown signal mid-scan doesn't abort it;
	// errors are logged, never fatal.
	go rt.RunBlockGCReconcileOnce(context.WithoutCancel(ctx))

	// Background auto-GC (#1433): periodically reclaims orphaned blocks on both
	// tiers so operators don't have to run `dfsctl store block gc` by hand.
	// Enabled by default; bound to ctx so it stops on shutdown.
	if cfg.GC.AutoGCEnabled() {
		rt.StartScheduledGC(ctx, cfg.GC.AutoInterval)
	} else {
		logger.Info("auto-GC disabled by config (gc.auto_enabled=false)")
	}

	// Configure runtime
	rt.SetShutdownTimeout(cfg.ShutdownTimeout)
	// Seed an operator-pinned machine SID (if configured) BEFORE Serve so the
	// machine SID is resolved before any adapter reads the SID mapper. Pinning
	// the same SID on every node yields identical local UID->SID encoding.
	rt.SetPinnedMachineSID(cfg.Identity.MachineSID)

	// Resolve identity-provider config: a DB row (managed via the API) wins over
	// the file/env config; on first boot the file/env config seeds the DB. This
	// sets the runtime's LDAP config and returns the effective Kerberos config to
	// hand to the adapter factory.
	effectiveKerberos := resolveIdentityProviders(ctx, cpStore, rt, cfg)

	// Build the single, process-wide NETLOGON authenticator before the adapter
	// factory so every SMB adapter instance shares it (one machine account per
	// process). The online-join provider persists its rotated secret in the
	// control-plane settings store, so it survives restarts. nlAuth is nil when
	// machine_account is disabled. nlRotation drives periodic password rotation
	// and is started below / stopped on shutdown.
	nlAuth, nlRotation := buildNetlogonAuthenticator(effectiveKerberos, newMachineSecretStore(cpStore))
	nlRotation.Start()
	// Single defer with deterministic ordering: stop the rotation loop FIRST so
	// no rotation can re-establish (and leak) the secure channel after Close, then
	// close the authenticator's cached channel.
	defer func() {
		nlRotation.Stop()
		if nlAuth != nil {
			nlAuth.Close(context.Background())
		}
	}()

	rt.SetAdapterFactory(createAdapterFactory(&effectiveKerberos, nlAuth))

	// Create and set API server
	apiServer, err := api.NewServer(cfg.ControlPlane, rt, cpStore, api.Timeouts{
		Restore:    cfg.Snapshot.RestoreHTTPTimeout,
		DrainStall: cfg.ControlPlane.DrainStallTimeout,
	})
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

		// Wait for the runtime to drain (snapshots -> StopAllAdapters -> flush
		// -> close stores -> stop API). The drain runs these stages serially
		// and most are individually bounded by the configured shutdown timeout,
		// but a wedged stage (e.g. a stuck store flush or an unbounded store
		// close) could otherwise block the process indefinitely. Under
		// Kubernetes that means the pod hangs until SIGKILL
		// at the end of its terminationGracePeriod, dropping clients abruptly —
		// exactly what #1313 is about. Cap the overall wait so the process
		// always exits on its own first.
		//
		// The factor mirrors the operator's shutdownStageMultiplier=3, which
		// sizes terminationGracePeriodSeconds as preStop(5) + 3*shutdownTimeout
		// + 10s. After SIGTERM the process therefore has 3*shutdownTimeout + 10s
		// before SIGKILL, so 3*shutdownTimeout + 5s self-exits just inside that
		// window while still letting the common multi-stage drain finish.
		shutdownDeadline := 3*cfg.ShutdownTimeout + 5*time.Second
		timer := time.NewTimer(shutdownDeadline)
		defer timer.Stop()
		select {
		case err := <-serverDone:
			if isExpectedShutdownErr(err) {
				logger.Info("Server stopped gracefully")
			} else {
				logger.Error("Server shutdown error", "error", err)
				return err
			}
		case <-timer.C:
			logger.Error("Graceful shutdown exceeded deadline; forcing exit",
				"deadline", shutdownDeadline)
			return fmt.Errorf("graceful shutdown timed out after %s", shutdownDeadline)
		}

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

// isExpectedShutdownErr reports whether err is the normal outcome of a
// SIGTERM-initiated graceful shutdown rather than a real failure. We cancel the
// root context on purpose, so context.Canceled — and the http.ErrServerClosed a
// listener returns once Shutdown is called — are expected. Per the logging
// convention (expected errors at Info/Debug, unexpected at Error), these must
// not be logged at ERROR: a clean shutdown emitting ERROR on every restart
// tripped alerting / log-based health checks (#1329).
func isExpectedShutdownErr(err error) bool {
	return err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, http.ErrServerClosed)
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

// resolveIdentityProviders reconciles identity-provider configuration between
// the file/env config and the control-plane database. A persisted row (managed
// over the API) takes precedence; when no row exists the file/env config seeds
// the database on first boot. It sets the runtime's LDAP config directly and
// returns the effective Kerberos config for the adapter factory.
//
// Note: the API does not manage Kerberos static IdentityMapping entries, so a
// DB-sourced Kerberos config carries none — those are configured via the
// separate /api/v1/identity-mappings routes.
func resolveIdentityProviders(ctx context.Context, cpStore store.Store, rt *runtime.Runtime, cfg *config.Config) config.KerberosConfig {
	// LDAP: DB row wins; else seed from file/env when enabled.
	if row, err := cpStore.GetIdentityProviderConfig(ctx, models.IdentityProviderTypeLDAP); err == nil {
		if lc, derr := ldap.UnmarshalStored([]byte(row.Config)); derr == nil {
			rt.SetLDAPConfig(lc)
			logger.Info("LDAP identity provider loaded from control-plane store",
				"enabled", lc.Enabled, "url", lc.URL, "base_dn", lc.BaseDN)
		} else {
			logger.Warn("failed to decode stored LDAP config; ignoring", "error", derr)
		}
	} else if errors.Is(err, models.ErrIdentityProviderConfigNotFound) {
		if cfg.LDAP.Enabled {
			lc := cfg.LDAP
			rt.SetLDAPConfig(&lc)
			if blob, merr := ldap.MarshalStored(&lc); merr != nil {
				logger.Warn("failed to marshal LDAP config for seeding", "error", merr)
			} else if perr := cpStore.PutIdentityProviderConfig(ctx, &models.IdentityProviderConfig{
				Type: models.IdentityProviderTypeLDAP, Enabled: lc.Enabled, Config: string(blob),
			}); perr != nil {
				logger.Warn("failed to seed LDAP config into store", "error", perr)
			}
			logger.Info("LDAP identity provider enabled (seeded from config file/env)",
				"url", lc.URL, "base_dn", lc.BaseDN, "idmap", lc.Idmap)
		}
	} else {
		logger.Warn("failed to read stored LDAP config; falling back to file/env", "error", err)
		if cfg.LDAP.Enabled {
			lc := cfg.LDAP
			rt.SetLDAPConfig(&lc)
		}
	}

	// Kerberos: DB row wins; else seed from file/env when enabled.
	kerberos := cfg.Kerberos
	if row, err := cpStore.GetIdentityProviderConfig(ctx, models.IdentityProviderTypeKerberos); err == nil {
		var dto handlers.KerberosConfigDTO
		if json.Unmarshal([]byte(row.Config), &dto) == nil {
			kerberos = kerberosDTOToConfig(dto)
			logger.Info("Kerberos identity provider loaded from control-plane store",
				"enabled", kerberos.Enabled, "service_principal", kerberos.ServicePrincipal)
		} else {
			logger.Warn("failed to decode stored Kerberos config; falling back to file/env")
		}
	} else if errors.Is(err, models.ErrIdentityProviderConfigNotFound) && cfg.Kerberos.Enabled {
		if blob, merr := json.Marshal(kerberosConfigToDTO(cfg.Kerberos)); merr == nil {
			if perr := cpStore.PutIdentityProviderConfig(ctx, &models.IdentityProviderConfig{
				Type: models.IdentityProviderTypeKerberos, Enabled: cfg.Kerberos.Enabled, Config: string(blob),
			}); perr != nil {
				logger.Warn("failed to seed Kerberos config into store", "error", perr)
			}
		}
	}

	// Overlay the operator/env-injected machine-account config onto the
	// effective (DB-sourced) Kerberos config. See mergeMachineAccountFromFile
	// for the precedence rules (#1392).
	kerberos = mergeMachineAccountFromFile(kerberos, cfg.Kerberos)

	// Overlay deployment-path fields (keytab / krb5.conf mount paths) from
	// file/env. These are filesystem-mount concerns the operator controls, not
	// API-managed identity policy: when a volume mount moves (e.g. an operator
	// upgrade relocating the keytab), the stale DB-row path must not win and
	// crash the server on a missing file. See overlayDeploymentPaths.
	kerberos = overlayDeploymentPaths(kerberos, cfg.Kerberos)

	// Seed the runtime's hot-reloadable NETLOGON machine credential from the
	// effective Kerberos machine-account config (#1325). nil disables passthrough.
	// The SMB adapter reads this on an identity-provider config change to rebuild
	// its secure channel without a restart. (Online-join supplies its own
	// credential via the RotationManager, so the seeded credential is the
	// offline/static one.)
	if cred, ok := netlogonCredentialFromConfig(kerberos); ok {
		rt.SetNetlogonCredential(&cred)
	} else {
		rt.SetNetlogonCredential(nil)
	}

	return kerberos
}

// mergeMachineAccountFromFile overlays the operator/env-injected machine-account
// configuration onto the effective Kerberos config (which is DB-sourced when a
// control-plane row exists, else the file/env config itself).
//
// Two precedence rules, both rooted in the fact that machine-account credentials
// arrive out-of-band (the online-join bind password and the offline machine
// secret are injected via the config file / DITTOFS_KERBEROS_MACHINE_ACCOUNT_*
// env), NOT through the API-managed Kerberos DTO that backs the DB row:
//
//   - Online-join carries a privileged LDAP bind password and is intentionally
//     never persisted in the DB DTO, so it is ALWAYS sourced from file/env.
//   - The offline machine account (account name + secret) is overlaid from
//     file/env only when the effective config does not already enable one. This
//     fixes #1392: a DB Kerberos row seeded before NTLM passthrough was
//     configured carries no machine_account and would otherwise overwrite (drop)
//     the file/env credential the operator injected, silently disabling
//     passthrough. A machine account configured explicitly via the API (present
//     and enabled in the DB row) still wins.
func mergeMachineAccountFromFile(effective, file config.KerberosConfig) config.KerberosConfig {
	effective.MachineAccount.OnlineJoin = file.MachineAccount.OnlineJoin

	if !effective.MachineAccount.Enabled && file.MachineAccount.Enabled {
		onlineJoin := effective.MachineAccount.OnlineJoin
		effective.MachineAccount = file.MachineAccount
		effective.MachineAccount.OnlineJoin = onlineJoin
	}
	return effective
}

// overlayDeploymentPaths lets the file/env config override the keytab and
// krb5.conf paths carried in a DB-sourced Kerberos config.
//
// Unlike realm / service principal / NetBIOS domain (API-managed identity
// policy, where the DB row is authoritative), these are filesystem-mount
// concerns owned by the deployment: where the orchestrator mounts the keytab
// and krb5.conf volumes. Persisting them as policy means a DB row seeded under
// one layout pins a stale absolute path; when an operator upgrade relocates the
// mount (e.g. /etc/dittofs/krb5.keytab -> /kerberos/dittofs.keytab) the server
// would crashloop on "open ...: no such file". So a non-empty file/env path
// always wins; an empty one leaves the DB value intact (pure-API deployments
// that never set a path are unaffected).
func overlayDeploymentPaths(effective, file config.KerberosConfig) config.KerberosConfig {
	if file.KeytabPath != "" {
		effective.KeytabPath = file.KeytabPath
	}
	if file.Krb5Conf != "" {
		effective.Krb5Conf = file.Krb5Conf
	}
	if file.MachineAccount.KeytabPath != "" {
		effective.MachineAccount.KeytabPath = file.MachineAccount.KeytabPath
	}
	return effective
}

func kerberosConfigToDTO(c config.KerberosConfig) handlers.KerberosConfigDTO {
	dto := handlers.KerberosConfigDTO{
		Enabled:          c.Enabled,
		KeytabPath:       c.KeytabPath,
		ServicePrincipal: c.ServicePrincipal,
		Realm:            c.Realm,
		NetBIOSDomain:    c.NetBIOSDomain,
		DNSDomain:        c.DNSDomain,
		Krb5Conf:         c.Krb5Conf,
		MaxContexts:      c.MaxContexts,
		MachineAccount: handlers.KerberosMachineAccountDTO{
			Enabled:     c.MachineAccount.Enabled,
			AccountName: c.MachineAccount.AccountName,
			Secret:      c.MachineAccount.Secret,
			KeytabPath:  c.MachineAccount.KeytabPath,
			DCAddresses: c.MachineAccount.DCAddresses,
		},
	}
	if c.MaxClockSkew > 0 {
		dto.MaxClockSkew = c.MaxClockSkew.String()
	}
	if c.ContextTTL > 0 {
		dto.ContextTTL = c.ContextTTL.String()
	}
	return dto
}

func kerberosDTOToConfig(dto handlers.KerberosConfigDTO) config.KerberosConfig {
	c := config.KerberosConfig{
		Enabled:          dto.Enabled,
		KeytabPath:       dto.KeytabPath,
		ServicePrincipal: dto.ServicePrincipal,
		Realm:            dto.Realm,
		NetBIOSDomain:    dto.NetBIOSDomain,
		DNSDomain:        dto.DNSDomain,
		Krb5Conf:         dto.Krb5Conf,
		MaxContexts:      dto.MaxContexts,
		MachineAccount: config.MachineAccountConfig{
			Enabled:     dto.MachineAccount.Enabled,
			AccountName: dto.MachineAccount.AccountName,
			Secret:      dto.MachineAccount.Secret,
			KeytabPath:  dto.MachineAccount.KeytabPath,
			DCAddresses: dto.MachineAccount.DCAddresses,
		},
	}
	if d, err := time.ParseDuration(dto.MaxClockSkew); err == nil {
		c.MaxClockSkew = d
	}
	if d, err := time.ParseDuration(dto.ContextTTL); err == nil {
		c.ContextTTL = d
	}
	// Restore the defaults config.MustLoad would have applied, since a
	// DB-sourced config bypasses ApplyDefaults.
	if c.MaxClockSkew == 0 {
		c.MaxClockSkew = 5 * time.Minute
	}
	if c.ContextTTL == 0 {
		c.ContextTTL = 8 * time.Hour
	}
	if c.MaxContexts == 0 {
		c.MaxContexts = 10000
	}
	return c
}

// createAdapterFactory returns a factory function that creates protocol adapters
// from configuration. This factory is used by Runtime to create adapters
// dynamically when loading from store or when created via API.
//
// nlAuth is the shared, process-wide NETLOGON authenticator (one machine
// account per process). It is built once by the caller — so the online-join
// provider joins / loads the persisted secret exactly once regardless of how
// many SMB adapter instances the runtime spins up — and injected here. It is
// nil when machine_account is disabled.
func createAdapterFactory(kerberosConfig *config.KerberosConfig, nlAuth *netlogon.Authenticator) runtime.AdapterFactory {
	return func(cfg *models.AdapterConfig) (runtime.ProtocolAdapter, error) {
		switch cfg.Type {
		case "nfs":
			return createNFSAdapter(cfg, kerberosConfig)
		case "smb":
			return createSMBAdapter(cfg, kerberosConfig, nlAuth)
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

	nfsCfg := nfs.NFSConfig{Enabled: true, Port: port}

	parsedConfig, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to parse adapter config: %w", err)
	}
	if tlsCfg, ok := parsedConfig["tls"].(map[string]any); ok {
		if v, ok := tlsCfg["cert_file"].(string); ok {
			nfsCfg.TLS.CertFile = v
		}
		if v, ok := tlsCfg["key_file"].(string); ok {
			nfsCfg.TLS.KeyFile = v
		}
		if v, ok := tlsCfg["client_ca"].(string); ok {
			nfsCfg.TLS.ClientCA = v
		}
		if v, ok := tlsCfg["min_version"].(string); ok {
			nfsCfg.TLS.MinVersion = v
		}
		if v, ok := tlsCfg["mode"].(string); ok {
			nfsCfg.TLS.Mode = v
		}
	}

	adapter := nfs.New(nfsCfg)
	if kerberosConfig != nil && kerberosConfig.Enabled {
		adapter.SetKerberosConfig(kerberosConfig)
	}
	return adapter, nil
}

func createSMBAdapter(cfg *models.AdapterConfig, kerberosConfig *config.KerberosConfig, nlAuth *netlogon.Authenticator) (runtime.ProtocolAdapter, error) {
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

	// Wire the shared NETLOGON authenticator for domain-controller NTLM
	// pass-through. It is built once by the caller (one machine account per
	// process) and injected, regardless of kerberosConfig.Enabled — an NTLM-only
	// / legacy deployment sets kerberos.enabled=false but still needs NETLOGON
	// passthrough when machine_account.enabled=true. nlAuth is nil when
	// MachineAccount.Enabled is false, so SetNetlogonAuthenticator no-ops then.
	if kerberosConfig != nil {
		// When NTLM pass-through is active, the SMB handler must advertise, in its
		// NTLM CHALLENGE TargetInfo, both the AD NetBIOS/DNS domain AND the AD
		// machine-account computer name. Domain clients echo these into their
		// NTLMv2 response; if the advertised domain is WORKGROUP or the computer
		// name is the bare OS hostname (not the machine account), Samba's NETLOGON
		// SamLogon rejects the otherwise-valid response (#1357). SetKerberosProvider
		// covers the SPNEGO-enabled path, but NTLM-only members (kerberos.enabled
		// =false) never reach it, so set it explicitly here. The computer name is
		// the same workstation name the NETLOGON secure channel authenticates as.
		if nlAuth != nil {
			smbAdapter.SetADDomain(kerberosConfig.NetBIOSDomain, kerberosConfig.DNSDomain, netbiosWorkstation(*kerberosConfig))
		}
		smbAdapter.SetNetlogonAuthenticator(nlAuth)
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
// secret-free notice: the generated password is unrecoverable, and because the
// admin user now exists a restart will not re-bootstrap it, so a known
// credential can only be pre-set at the next fresh bootstrap.
func emitAdminPassword(password string) {
	if isTerminal(os.Stdout.Fd()) {
		fmt.Printf("\n*** IMPORTANT: Admin user created with password: %s ***\n", password)
		fmt.Println("Please save this password. It will not be shown again.")
		fmt.Println()
		return
	}
	// Daemon / non-interactive: never write the secret to the log.
	logger.Warn("Admin user created with a generated password, which is NOT recoverable in " +
		"background mode (stdout is not a terminal, so the password is not written to the log). " +
		"The admin user now exists, so restarting with DITTOFS_ADMIN_INITIAL_PASSWORD or " +
		"admin.password_hash set will NOT change it. To obtain a known credential, remove the " +
		"admin user (reset the control-plane database) and re-bootstrap with one of those set, " +
		"or bootstrap with 'dfs start --foreground' in a terminal to have it printed once.")
}

// formatLegacyLayoutDirective renders the multi-line operator directive
// printed when LoadSharesFromStore surfaces ErrLegacyLayoutDetected.
// The full wrapped error message (`share "<name>": share <path>:
// blockstore: legacy .blk layout detected (run `dfs migrate-to-cas`)`)
// is embedded verbatim so the operator sees BOTH the share name AND
// the offending path without fragile substring extraction.
func formatLegacyLayoutDirective(err error) string {
	return fmt.Sprintf(`Detected legacy .blk layout: %s.
This release no longer ships the .blk migration tool. Migrate the share
with dittofs v0.21 or earlier first:
    dfs migrate-to-cas --share <name>   (dittofs <= v0.21)
then upgrade; the cas->blocks conversion runs automatically at startup.`, err)
}
