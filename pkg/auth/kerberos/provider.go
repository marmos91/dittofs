package kerberos

import (
	"fmt"
	"os"
	"strings"
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

	// AD domain identity (AD-4). All optional: empty values mean the server
	// behaves as a standalone server (the SMB layer falls back to WORKGROUP).
	// These are immutable after construction (derived from config once) and so
	// are not guarded by mu.
	realm         string // Kerberos realm, e.g. CONTOSO.COM
	netbiosDomain string // NetBIOS short domain, e.g. CONTOSO ("" => standalone)
	dnsDomain     string // DNS domain, e.g. contoso.com

	mu sync.RWMutex
}

// NewProvider creates a new Kerberos provider from configuration.
//
// It loads the keytab file and krb5.conf at startup, then starts a KeytabManager
// that polls for keytab file changes every 60 seconds.
//
// Environment variables take precedence over config file values:
//   - DITTOFS_KERBEROS_KEYTAB overrides KeytabPath (also DITTOFS_KERBEROS_KEYTAB_PATH for compat)
//   - DITTOFS_KERBEROS_PRINCIPAL overrides ServicePrincipal (also DITTOFS_KERBEROS_SERVICE_PRINCIPAL)
//   - DITTOFS_KERBEROS_KRB5CONF overrides Krb5Conf
func NewProvider(cfg *dconfig.KerberosConfig) (*Provider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("kerberos config is nil")
	}

	keytabPath := resolveKeytabPath(cfg.KeytabPath)
	if keytabPath == "" {
		return nil, fmt.Errorf("kerberos keytab path not configured (set keytab_path or DITTOFS_KERBEROS_KEYTAB)")
	}

	servicePrincipal := resolveServicePrincipal(cfg.ServicePrincipal)
	if servicePrincipal == "" {
		return nil, fmt.Errorf("kerberos service principal not configured (set service_principal or DITTOFS_KERBEROS_PRINCIPAL)")
	}

	krb5ConfPath := resolveKrb5ConfPath(cfg.Krb5Conf)

	kt, err := loadKeytab(keytabPath)
	if err != nil {
		return nil, fmt.Errorf("load keytab %s: %w", keytabPath, err)
	}

	krbCfg, err := loadKrb5Conf(krb5ConfPath)
	if err != nil {
		return nil, fmt.Errorf("load krb5.conf %s: %w", krb5ConfPath, err)
	}

	// Resolve the AD domain identity. Realm defaults to the principal's
	// "@REALM" suffix; DNSDomain defaults to the lowercased realm. NetBIOSDomain
	// is never derived — when empty the SMB layer uses WORKGROUP (standalone).
	// ApplyDefaults already fills these when the config flows through it, but
	// resolve them here too so direct NewProvider callers (e.g. tests) and any
	// path that bypasses ApplyDefaults still get consistent values.
	realm := cfg.Realm
	if realm == "" {
		realm = extractRealm(servicePrincipal)
	}
	dnsDomain := cfg.DNSDomain
	if dnsDomain == "" && realm != "" {
		dnsDomain = strings.ToLower(realm)
	}

	p := &Provider{
		keytab:           kt,
		krb5Conf:         krbCfg,
		servicePrincipal: servicePrincipal,
		maxClockSkew:     cfg.MaxClockSkew,
		keytabPath:       keytabPath,
		realm:            realm,
		netbiosDomain:    cfg.NetBIOSDomain,
		dnsDomain:        dnsDomain,
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

// Realm returns the Kerberos realm the server is joined to (e.g. CONTOSO.COM),
// or "" if it could not be determined. Immutable after construction.
func (p *Provider) Realm() string {
	return p.realm
}

// NetBIOSDomain returns the configured NetBIOS short domain (e.g. CONTOSO), or
// "" when the server is standalone (in which case the SMB layer advertises and
// uses WORKGROUP). Immutable after construction.
func (p *Provider) NetBIOSDomain() string {
	return p.netbiosDomain
}

// DNSDomain returns the DNS domain (e.g. contoso.com), defaulting to the
// lowercased realm, or "" if neither was configured/derivable. Immutable after
// construction.
func (p *Provider) DNSDomain() string {
	return p.dnsDomain
}

// extractRealm returns the "@REALM" suffix of a service principal
// (e.g. "nfs/host@CONTOSO.COM" -> "CONTOSO.COM"), or "" if absent.
func extractRealm(principal string) string {
	if idx := strings.LastIndex(principal, "@"); idx >= 0 && idx < len(principal)-1 {
		return principal[idx+1:]
	}
	return ""
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
// This enables keytab rotation without server restart. Active contexts
// continue using the old keytab; new contexts use the new one.
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

// Close stops the KeytabManager's polling goroutine. Safe to call multiple times.
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
