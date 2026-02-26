package kerberos

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth"
	dconfig "github.com/marmos91/dittofs/pkg/config"
)

// Provider manages Kerberos keytab, krb5.conf, and service principal state.
//
// Provider implements the auth.AuthProvider interface, allowing it to be used
// in an auth.Authenticator chain alongside other authentication mechanisms.
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

// Compile-time check that Provider implements auth.AuthProvider.
var _ auth.AuthProvider = (*Provider)(nil)

// CanHandle returns true if the token is a Kerberos/SPNEGO authentication token.
//
// Detection is based on ASN.1 structure:
//   - SPNEGO tokens start with ASN.1 Application tag 0x60 followed by the
//     SPNEGO OID (1.3.6.1.5.5.2)
//   - Raw Kerberos AP-REQ tokens start with ASN.1 Application tag [14] (0x6E)
//
// This is a fast check that does not perform full token parsing.
func (p *Provider) CanHandle(token []byte) bool {
	if len(token) < 2 {
		return false
	}

	// SPNEGO initiation token: starts with ASN.1 Application [0] (0x60)
	// followed by length and SPNEGO OID 1.3.6.1.5.5.2
	if token[0] == 0x60 {
		// Check for SPNEGO OID prefix: 06 06 2b 06 01 05 05 02
		// (OID tag + length + OID bytes for 1.3.6.1.5.5.2)
		spnegoOID := []byte{0x06, 0x06, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x02}
		// The OID appears after the outer length encoding (variable length)
		for i := 2; i < len(token)-len(spnegoOID); i++ {
			if token[i] == spnegoOID[0] {
				match := true
				for j := 0; j < len(spnegoOID) && i+j < len(token); j++ {
					if token[i+j] != spnegoOID[j] {
						match = false
						break
					}
				}
				if match {
					return true
				}
			}
		}
	}

	// Raw Kerberos AP-REQ: ASN.1 Application [14] (0x6E)
	if token[0] == 0x6E {
		return true
	}

	return false
}

// Authenticate processes a Kerberos/SPNEGO token and returns an AuthResult.
//
// This is a high-level entry point for the auth.AuthProvider interface.
// The actual RPCSEC_GSS and SPNEGO processing is handled by protocol-specific
// code (internal/adapter/nfs/rpc/gss/ for NFS, internal/adapter/smb/auth/ for SMB).
// This method provides a basic Kerberos token validation using the configured
// keytab and service principal.
//
// For full protocol integration, use the protocol-specific authenticators that
// wrap this provider.
func (p *Provider) Authenticate(_ context.Context, token []byte) (*auth.AuthResult, error) {
	if !p.CanHandle(token) {
		return nil, auth.ErrUnsupportedMechanism
	}

	// The Provider itself manages keytab state; full Kerberos token validation
	// (AP-REQ verification, SPNEGO negotiation) is handled by the protocol-specific
	// layers that use this provider (e.g., gss.Krb5Verifier for NFS, SMB auth handler).
	//
	// Return a basic result indicating Kerberos mechanism was identified.
	// Protocol-specific code should call the GSS/SPNEGO processors directly
	// for full authentication with ticket validation.
	return &auth.AuthResult{
		Identity: auth.Identity{
			Attributes: map[string]string{
				"mechanism": "kerberos",
			},
		},
		Authenticated: false, // Requires protocol-specific processing for full auth
		Provider:      p.Name(),
	}, nil
}

// Name returns the provider name for logging and diagnostics.
func (p *Provider) Name() string {
	return "kerberos"
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
