package kerberos

import (
	"fmt"
	"os"
	"sync"
	"time"

	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/marmos91/dittofs/internal/logger"
	dconfig "github.com/marmos91/dittofs/pkg/config"
)

// Provider manages Kerberos keytab, krb5.conf, and service principal state.
//
// It is the shared Kerberos resource used by the RPCSEC_GSS context manager
// and other components that need access to the Kerberos configuration.
//
// Thread Safety: All methods are safe for concurrent use. The keytab can be
// hot-reloaded at runtime via ReloadKeytab() without disrupting active contexts.
type Provider struct {
	keytab           *keytab.Keytab
	krb5Conf         *krb5config.Config
	servicePrincipal string
	maxClockSkew     time.Duration
	keytabPath       string
	keytabManager    *KeytabManager
	mu               sync.RWMutex
}

// NewProvider creates a new Kerberos provider from configuration.
//
// The provider loads the keytab file and krb5.conf at startup, then starts
// a KeytabManager that polls for keytab file changes every 60 seconds.
//
// Environment variables take precedence over config file values:
//   - DITTOFS_KERBEROS_KEYTAB overrides KeytabPath (also DITTOFS_KERBEROS_KEYTAB_PATH for compat)
//   - DITTOFS_KERBEROS_PRINCIPAL overrides ServicePrincipal (also DITTOFS_KERBEROS_SERVICE_PRINCIPAL)
//   - DITTOFS_KERBEROS_KRB5CONF overrides Krb5Conf
//
// Parameters:
//   - cfg: Kerberos configuration (from pkg/config)
//
// Returns:
//   - *Provider: Initialized provider with loaded keytab, krb5.conf, and active hot-reload
//   - error: If keytab or krb5.conf cannot be loaded
func NewProvider(cfg *dconfig.KerberosConfig) (*Provider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("kerberos config is nil")
	}

	// Resolve keytab path (env var takes precedence via resolveKeytabPath)
	keytabPath := resolveKeytabPath(cfg.KeytabPath)
	// Also support legacy env var DITTOFS_KERBEROS_KEYTAB_PATH
	if keytabPath == cfg.KeytabPath {
		if envPath := os.Getenv("DITTOFS_KERBEROS_KEYTAB_PATH"); envPath != "" {
			keytabPath = envPath
		}
	}
	if keytabPath == "" {
		return nil, fmt.Errorf("kerberos keytab path not configured (set keytab_path or DITTOFS_KERBEROS_KEYTAB)")
	}

	// Resolve service principal (env var takes precedence via resolveServicePrincipal)
	servicePrincipal := resolveServicePrincipal(cfg.ServicePrincipal)
	// Also support legacy env var DITTOFS_KERBEROS_SERVICE_PRINCIPAL
	if servicePrincipal == cfg.ServicePrincipal {
		if envSPN := os.Getenv("DITTOFS_KERBEROS_SERVICE_PRINCIPAL"); envSPN != "" {
			servicePrincipal = envSPN
		}
	}
	if servicePrincipal == "" {
		return nil, fmt.Errorf("kerberos service principal not configured (set service_principal or DITTOFS_KERBEROS_PRINCIPAL)")
	}

	// Resolve krb5.conf path (env var takes precedence)
	krb5ConfPath := cfg.Krb5Conf
	if envConf := os.Getenv("DITTOFS_KERBEROS_KRB5CONF"); envConf != "" {
		krb5ConfPath = envConf
	}
	if krb5ConfPath == "" {
		krb5ConfPath = "/etc/krb5.conf"
	}

	// Load keytab
	kt, err := loadKeytab(keytabPath)
	if err != nil {
		return nil, fmt.Errorf("load keytab %s: %w", keytabPath, err)
	}

	// Load krb5.conf
	krbCfg, err := loadKrb5Conf(krb5ConfPath)
	if err != nil {
		return nil, fmt.Errorf("load krb5.conf %s: %w", krb5ConfPath, err)
	}

	p := &Provider{
		keytab:           kt,
		krb5Conf:         krbCfg,
		servicePrincipal: servicePrincipal,
		maxClockSkew:     cfg.MaxClockSkew,
		keytabPath:       keytabPath,
	}

	// Create and start keytab manager for hot-reload
	km := NewKeytabManager(keytabPath, p)
	if err := km.Start(); err != nil {
		// Non-fatal: log warning but continue (hot-reload just won't work)
		// This can happen if the file is deleted between load and start
		logger.Warn("Keytab hot-reload failed to start, continuing without it",
			"path", keytabPath, "error", err)
	}
	p.keytabManager = km

	return p, nil
}

// Keytab returns the current keytab (thread-safe read).
func (p *Provider) Keytab() *keytab.Keytab {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.keytab
}

// ServicePrincipal returns the configured service principal name.
func (p *Provider) ServicePrincipal() string {
	return p.servicePrincipal
}

// MaxClockSkew returns the maximum allowed clock skew.
func (p *Provider) MaxClockSkew() time.Duration {
	return p.maxClockSkew
}

// Krb5Config returns the loaded Kerberos configuration.
func (p *Provider) Krb5Config() *krb5config.Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.krb5Conf
}

// ReloadKeytab re-reads the keytab file and atomically swaps it.
//
// This enables keytab rotation without server restart. Active contexts
// continue using the old keytab for verification; new contexts use the
// new keytab.
//
// Returns:
//   - error: If the new keytab cannot be loaded (old keytab remains active)
func (p *Provider) ReloadKeytab() error {
	kt, err := loadKeytab(p.keytabPath)
	if err != nil {
		return fmt.Errorf("reload keytab %s: %w", p.keytabPath, err)
	}

	p.mu.Lock()
	p.keytab = kt
	p.mu.Unlock()

	return nil
}

// Close releases resources held by the provider.
//
// This stops the KeytabManager's polling goroutine. Safe to call multiple times.
func (p *Provider) Close() error {
	if p.keytabManager != nil {
		p.keytabManager.Stop()
	}
	return nil
}

// loadKeytab reads and parses a keytab file.
func loadKeytab(path string) (*keytab.Keytab, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keytab file: %w", err)
	}

	kt := keytab.New()
	if err := kt.Unmarshal(data); err != nil {
		return nil, fmt.Errorf("parse keytab: %w", err)
	}

	return kt, nil
}

// loadKrb5Conf reads and parses a Kerberos configuration file.
func loadKrb5Conf(path string) (*krb5config.Config, error) {
	cfg, err := krb5config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("parse krb5.conf: %w", err)
	}

	return cfg, nil
}
