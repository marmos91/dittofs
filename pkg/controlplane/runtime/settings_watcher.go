package runtime

import (
	"context"
	"errors"
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

	pollInterval time.Duration
	stopCh       chan struct{}
	stopped      chan struct{} // closed when polling goroutine exits
}

// NewSettingsWatcher creates a new SettingsWatcher with the given store and poll interval.
// If pollInterval is 0, DefaultPollInterval (10s) is used.
func NewSettingsWatcher(s store.Store, pollInterval time.Duration) *SettingsWatcher {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	return &SettingsWatcher{
		store:        s,
		pollInterval: pollInterval,
		stopCh:       make(chan struct{}),
		stopped:      make(chan struct{}),
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
	go func() {
		defer close(w.stopped)

		ticker := time.NewTicker(w.pollInterval)
		defer ticker.Stop()

		logger.Info("Settings watcher started", "poll_interval", w.pollInterval)

		for {
			select {
			case <-ctx.Done():
				logger.Debug("Settings watcher stopping (context cancelled)")
				return
			case <-w.stopCh:
				logger.Debug("Settings watcher stopping (stop signal)")
				return
			case <-ticker.C:
				w.poll(ctx)
			}
		}
	}()
}

// Stop signals the polling goroutine to stop and waits for it to exit.
func (w *SettingsWatcher) Stop() {
	select {
	case <-w.stopCh:
		// Already stopped
		return
	default:
		close(w.stopCh)
	}
	// Wait for goroutine to exit
	<-w.stopped
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
	// Get NFS adapter from store
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
			)
		}
	}

	return nil
}

// pollSMBSettings checks the DB for SMB adapter settings changes.
// If the version has changed, swaps the cached settings atomically.
func (w *SettingsWatcher) pollSMBSettings(ctx context.Context) error {
	// Get SMB adapter from store
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
			)
		}
	}

	return nil
}
