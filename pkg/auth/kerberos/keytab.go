package kerberos

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// keytabPollInterval is the interval at which the keytab file is polled for changes.
const keytabPollInterval = 60 * time.Second

// KeytabManager watches a keytab file for changes and triggers hot-reload.
//
// It uses a polling approach (checking file modification time every 60 seconds)
// rather than fsnotify because polling is more reliable across platforms for
// keytab files, which may be atomically replaced (e.g., via rename) by key
// management tools like kadmin or k5srvutil.
//
// Thread Safety: All methods are safe for concurrent use.
type KeytabManager struct {
	path     string
	provider *Provider
	stopCh   chan struct{}
	mu       sync.Mutex
	lastMod  time.Time
}

// NewKeytabManager creates a new keytab file manager (not yet started).
func NewKeytabManager(path string, provider *Provider) *KeytabManager {
	return &KeytabManager{
		path:     path,
		provider: provider,
		stopCh:   make(chan struct{}),
	}
}

// Start begins polling the keytab file for changes.
// It validates the file exists, records its initial modification time, then
// starts a background goroutine that polls every 60 seconds.
func (km *KeytabManager) Start() error {
	km.mu.Lock()
	defer km.mu.Unlock()

	// Validate the keytab file exists and is readable
	info, err := os.Stat(km.path)
	if err != nil {
		return fmt.Errorf("keytab file not accessible: %w", err)
	}

	km.lastMod = info.ModTime()

	// Start the polling goroutine
	go km.pollLoop()

	logger.Info("Keytab hot-reload started",
		"path", km.path,
		"poll_interval", keytabPollInterval.String(),
	)

	return nil
}

// Stop stops the polling goroutine.
//
// This is safe to call multiple times or on a manager that was never started.
func (km *KeytabManager) Stop() {
	select {
	case <-km.stopCh:
		// Already stopped
	default:
		close(km.stopCh)
	}
}

// pollLoop runs the periodic file change check.
func (km *KeytabManager) pollLoop() {
	ticker := time.NewTicker(keytabPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			km.checkAndReload()
		case <-km.stopCh:
			return
		}
	}
}

// checkAndReload checks if the keytab file has changed and reloads if needed.
func (km *KeytabManager) checkAndReload() {
	km.mu.Lock()
	defer km.mu.Unlock()

	info, err := os.Stat(km.path)
	if err != nil {
		logger.Error("Keytab file stat failed",
			"path", km.path,
			"error", err,
		)
		return
	}

	modTime := info.ModTime()
	if modTime.Equal(km.lastMod) {
		return // No change
	}

	// File has changed, attempt reload
	if err := km.provider.ReloadKeytab(); err != nil {
		logger.Error("Keytab reload failed",
			"path", km.path,
			"error", err,
		)
		return
	}

	km.lastMod = modTime
	logger.Info("Keytab reloaded successfully",
		"path", km.path,
	)
}

// resolveKeytabPath resolves the keytab path with environment variable override.
//
// Resolution order (highest priority first):
//  1. DITTOFS_KERBEROS_KEYTAB env var (preferred)
//  2. DITTOFS_KERBEROS_KEYTAB_PATH env var (alternative name)
//  3. configPath from configuration file
func resolveKeytabPath(configPath string) string {
	if envPath := os.Getenv("DITTOFS_KERBEROS_KEYTAB"); envPath != "" {
		return envPath
	}
	if envPath := os.Getenv("DITTOFS_KERBEROS_KEYTAB_PATH"); envPath != "" {
		return envPath
	}
	return configPath
}

// resolveServicePrincipal resolves the service principal with environment variable override.
//
// Resolution order (highest priority first):
//  1. DITTOFS_KERBEROS_PRINCIPAL env var
//  2. DITTOFS_KERBEROS_SERVICE_PRINCIPAL env var (alternative name)
//  3. configPrincipal from configuration file
func resolveServicePrincipal(configPrincipal string) string {
	if envSPN := os.Getenv("DITTOFS_KERBEROS_PRINCIPAL"); envSPN != "" {
		return envSPN
	}
	if envSPN := os.Getenv("DITTOFS_KERBEROS_SERVICE_PRINCIPAL"); envSPN != "" {
		return envSPN
	}
	return configPrincipal
}

// resolveKrb5ConfPath resolves the krb5.conf path with environment variable override.
//
// Resolution order (highest priority first):
//  1. DITTOFS_KERBEROS_KRB5CONF env var
//  2. configPath from configuration file
//  3. Default: /etc/krb5.conf
func resolveKrb5ConfPath(configPath string) string {
	if envPath := os.Getenv("DITTOFS_KERBEROS_KRB5CONF"); envPath != "" {
		return envPath
	}
	if configPath != "" {
		return configPath
	}
	return "/etc/krb5.conf"
}
