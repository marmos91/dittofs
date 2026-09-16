package runtime

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// DefaultPollInterval is the default interval for polling the DB for settings changes.
const DefaultPollInterval = 10 * time.Second

// SettingsWatcher polls the database for adapter settings changes and provides
// thread-safe access to cached settings for protocol adapters.
//
// Design:
//   - Polls DB every pollInterval (default 10s) for settings changes
//   - Uses monotonic version counter for change detection (not timestamps)
//   - Atomic pointer swap for thread safety: entire settings struct is replaced
//   - Readers acquire RLock and get a pointer; callers must NOT mutate the returned struct
//   - Security policy changes logged at INFO level for audit trail
//
// Thread safety:
//   - Writers (poll goroutine): acquire mu.Lock(), swap entire struct pointer
//   - Readers (adapter goroutines): acquire mu.RLock(), read pointer
type SettingsWatcher struct {
	mu    sync.RWMutex
	store store.Store

	// Cached settings (read by adapters via GetNFS/SMBSettings)
	nfsSettings *models.NFSAdapterSettings
	smbSettings *models.SMBAdapterSettings

	// Last known version for change detection
	nfsVersion int
	smbVersion int

	// Callbacks invoked when settings change (after initial load)
	nfsCallbacks []func(*models.NFSAdapterSettings)
	smbCallbacks []func(*models.SMBAdapterSettings)

	pollInterval time.Duration

	// lifecycleMu guards stopCh and stopped, which Start replaces and Stop
	// closes. Stop is reached from two shutdown paths that both bound their
	// wait and keep running after it — the lifecycle drain and the runtime's
	// startup drain — so an unsynchronized check-then-close here is two
	// goroutines racing to close the same channel, which panics the process
	// during the shutdown it was supposed to make orderly.
	lifecycleMu sync.Mutex
	stopCh      chan struct{}
	stopped     chan struct{} // closed when polling goroutine exits
	// cancelPoll aborts a poll already inside the control-plane store. Without
	// it Stop can only stop WAITING for that poll, which is not the same as
	// stopping it: the caller then closes the store under a query that is still
	// running, which is the outcome the join exists to prevent.
	cancelPoll context.CancelFunc
}

// OnNFSSettingsChange registers a callback invoked whenever NFS settings change
// (after the initial load). Used by the NFS adapter to refresh cached state such
// as the blocked-operations set and filesystem capabilities without re-reading
// live settings on every request.
func (w *SettingsWatcher) OnNFSSettingsChange(cb func(*models.NFSAdapterSettings)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nfsCallbacks = append(w.nfsCallbacks, cb)
}

// OnSMBSettingsChange registers a callback invoked whenever SMB settings change.
func (w *SettingsWatcher) OnSMBSettingsChange(cb func(*models.SMBAdapterSettings)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.smbCallbacks = append(w.smbCallbacks, cb)
}

// NewSettingsWatcher creates a new SettingsWatcher with the given store and poll interval.
// If pollInterval is 0, DefaultPollInterval (10s) is used.
func NewSettingsWatcher(s store.Store, pollInterval time.Duration) *SettingsWatcher {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	// stopped starts already-closed so that Stop() called before Start()
	// returns immediately instead of deadlocking on <-w.stopped (no goroutine
	// would ever close it). Start() re-creates it as a fresh open channel.
	stopped := make(chan struct{})
	close(stopped)
	return &SettingsWatcher{
		store:        s,
		pollInterval: pollInterval,
		stopCh:       make(chan struct{}),
		stopped:      stopped,
	}
}

// LoadInitial performs an initial load of settings from the database.
// This should be called once at startup to populate the cache before serving begins.
// Returns an error if the DB is unreachable or settings cannot be loaded.
func (w *SettingsWatcher) LoadInitial(ctx context.Context) error {
	if err := w.pollNFSSettings(ctx); err != nil {
		logger.Warn("Settings watcher: failed to load initial NFS settings", "error", err)
		// Non-fatal: NFS adapter may not exist yet
	}

	if err := w.pollSMBSettings(ctx); err != nil {
		logger.Warn("Settings watcher: failed to load initial SMB settings", "error", err)
		// Non-fatal: SMB adapter may not exist yet
	}

	return nil
}

// Start begins the background polling goroutine. On each tick it checks for
// NFS and SMB adapter settings changes and updates the cache atomically.
//
// The goroutine continues until Stop() is called or the context is cancelled.
func (w *SettingsWatcher) Start(ctx context.Context) {
	// Re-create the channels as fresh, open channels. stopped was created
	// already-closed (so Stop-before-Start is safe); the goroutine below will
	// close it on exit. Resetting stopCh allows a Start→Stop→Start→Stop cycle.
	// Derived, so Stop can cancel the polls without disturbing the caller's
	// context — which on a startup error is still very much alive.
	pollCtx, cancelPoll := context.WithCancel(ctx)

	w.lifecycleMu.Lock()
	w.stopped = make(chan struct{})
	w.stopCh = make(chan struct{})
	w.cancelPoll = cancelPoll
	stopCh, stopped := w.stopCh, w.stopped
	w.lifecycleMu.Unlock()

	go func() {
		defer close(stopped)
		defer cancelPoll()

		ticker := time.NewTicker(w.pollInterval)
		defer ticker.Stop()

		logger.Info("Settings watcher started", "poll_interval", w.pollInterval)

		for {
			select {
			case <-pollCtx.Done():
				logger.Debug("Settings watcher stopping (context cancelled)")
				return
			case <-stopCh:
				logger.Debug("Settings watcher stopping (stop signal)")
				return
			case <-ticker.C:
				w.pollWithRecover(pollCtx)
			}
		}
	}()
}

// Stop signals the polling goroutine to stop and waits for it to exit.
func (w *SettingsWatcher) Stop() {
	w.lifecycleMu.Lock()
	stopCh, stopped, cancelPoll := w.stopCh, w.stopped, w.cancelPoll
	select {
	case <-stopCh:
		// Already signalled by an earlier Stop, which may still be waiting for
		// the goroutine. Fall through to the same wait rather than returning:
		// a caller that gets an immediate return believes it joined.
	default:
		close(stopCh)
	}
	w.lifecycleMu.Unlock()

	// Cancel before waiting. A poll already inside the store returns on a
	// cancelled context, so the wait below is a join that completes rather than
	// one a caller has to abandon — and abandoning it is what leaves the store
	// closing under a live query.
	if cancelPoll != nil {
		cancelPoll()
	}

	<-stopped
	logger.Debug("Settings watcher stopped")
}

// GetNFSSettings returns the cached NFS adapter settings.
// The returned pointer must NOT be mutated by callers.
// Returns nil if no NFS settings have been loaded yet.
func (w *SettingsWatcher) GetNFSSettings() *models.NFSAdapterSettings {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.nfsSettings
}

// GetSMBSettings returns the cached SMB adapter settings.
// The returned pointer must NOT be mutated by callers.
// Returns nil if no SMB settings have been loaded yet.
func (w *SettingsWatcher) GetSMBSettings() *models.SMBAdapterSettings {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.smbSettings
}

// pollWithRecover runs one poll cycle, recovering from any panic so a single
// bad tick (e.g. a panicking change callback) cannot silently kill the
// background goroutine and freeze settings hot-reload for the process lifetime.
// The next ticker tick re-enters the loop and resumes polling.
func (w *SettingsWatcher) pollWithRecover(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Settings watcher: poll panicked, recovering and continuing",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	w.poll(ctx)
}

// poll checks both NFS and SMB settings for changes.
func (w *SettingsWatcher) poll(ctx context.Context) {
	if err := w.pollNFSSettings(ctx); err != nil {
		logger.Warn("Settings watcher: failed to poll NFS settings", "error", err)
	}
	if err := w.pollSMBSettings(ctx); err != nil {
		logger.Warn("Settings watcher: failed to poll SMB settings", "error", err)
	}
}

// pollNFSSettings checks the DB for NFS adapter settings changes.
// If the version has changed, swaps the cached settings atomically.
func (w *SettingsWatcher) pollNFSSettings(ctx context.Context) error {
	adapter, err := w.store.GetAdapter(ctx, "nfs")
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			return nil // NFS adapter may not exist
		}
		return err
	}

	settings, err := w.store.GetNFSAdapterSettings(ctx, adapter.ID)
	if err != nil {
		return err
	}

	w.mu.RLock()
	currentVersion := w.nfsVersion
	w.mu.RUnlock()

	if settings.Version != currentVersion {
		// Version changed - swap atomically
		w.mu.Lock()
		w.nfsSettings = settings
		w.nfsVersion = settings.Version
		w.mu.Unlock()

		if currentVersion > 0 {
			// Log only after initial load (not on first poll)
			logger.Info("NFS adapter settings reloaded",
				"version", settings.Version,
				"lease_time", settings.LeaseTime,
				"grace_period", settings.GracePeriod,
				"delegations_enabled", settings.DelegationsEnabled,
				"max_connections", settings.MaxConnections,
				"max_compound_ops", settings.MaxCompoundOps,
				"blocked_operations", settings.GetBlockedOperations(),
			)

			// Notify registered callbacks so adapters can refresh cached state.
			w.mu.RLock()
			cbs := w.nfsCallbacks
			w.mu.RUnlock()
			for _, cb := range cbs {
				cb(settings)
			}
		}
	}

	return nil
}

// RefreshSMBSettings forces a synchronous re-read of SMB adapter settings
// from the database, bypassing the periodic poll cadence. Use when adapter
// lifecycle events (enable/disable, restart) must see settings updates that
// happened within the current poll window. Returns the same errors as the
// internal poll and is a no-op if the settings version is unchanged.
func (w *SettingsWatcher) RefreshSMBSettings(ctx context.Context) error {
	return w.pollSMBSettings(ctx)
}

// RefreshNFSSettings forces a synchronous re-read of NFS adapter settings
// from the database, bypassing the periodic poll cadence. Use when adapter
// lifecycle events (enable/disable, restart) must see settings updates that
// happened within the current poll window. Returns the same errors as the
// internal poll and is a no-op if the settings version is unchanged.
func (w *SettingsWatcher) RefreshNFSSettings(ctx context.Context) error {
	return w.pollNFSSettings(ctx)
}

// pollSMBSettings checks the DB for SMB adapter settings changes.
// If the version has changed, swaps the cached settings atomically.
func (w *SettingsWatcher) pollSMBSettings(ctx context.Context) error {
	adapter, err := w.store.GetAdapter(ctx, "smb")
	if err != nil {
		if errors.Is(err, models.ErrAdapterNotFound) {
			return nil // SMB adapter may not exist
		}
		return err
	}

	settings, err := w.store.GetSMBAdapterSettings(ctx, adapter.ID)
	if err != nil {
		return err
	}

	w.mu.RLock()
	currentVersion := w.smbVersion
	w.mu.RUnlock()

	if settings.Version != currentVersion {
		// Version changed - swap atomically
		w.mu.Lock()
		w.smbSettings = settings
		w.smbVersion = settings.Version
		w.mu.Unlock()

		if currentVersion > 0 {
			// Log only after initial load (not on first poll)
			logger.Info("SMB adapter settings reloaded",
				"version", settings.Version,
				"session_timeout", settings.SessionTimeout,
				"oplock_break_timeout", settings.OplockBreakTimeout,
				"max_connections", settings.MaxConnections,
				"max_sessions", settings.MaxSessions,
				"enable_encryption", settings.EnableEncryption,
				"signing", settings.Signing,
				"directory_leasing_enabled", settings.DirectoryLeasingEnabled,
			)

			// Notify registered callbacks
			w.mu.RLock()
			cbs := w.smbCallbacks
			w.mu.RUnlock()
			for _, cb := range cbs {
				cb(settings)
			}
		}
	}

	return nil
}
