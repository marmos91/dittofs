package kerberos

import (
	"bytes"
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

// Compile-time check that Provider implements auth.AuthProvider.
var _ auth.AuthProvider = (*Provider)(nil)

// spnegoOID is the ASN.1 encoded OID for SPNEGO (1.3.6.1.5.5.2):
// OID tag (0x06), length (0x06), then the OID bytes.
var spnegoOID = []byte{0x06, 0x06, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x02}

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

	// SPNEGO initiation token: ASN.1 Application [0] (0x60) containing the SPNEGO OID
	if token[0] == 0x60 && bytes.Contains(token, spnegoOID) {
		return true
	}

	// Raw Kerberos AP-REQ: ASN.1 Application [14] (0x6E)
	return token[0] == 0x6E
}

// Authenticate identifies a Kerberos/SPNEGO token and returns a preliminary AuthResult.
//
// Full token validation (AP-REQ verification, SPNEGO negotiation) is handled by
// protocol-specific layers (gss.Krb5Verifier for NFS, SMB auth handler). This
// method only identifies the mechanism; callers should use the protocol-specific
// authenticators for full authentication.
func (p *Provider) Authenticate(_ context.Context, token []byte) (*auth.AuthResult, error) {
	if !p.CanHandle(token) {
		return nil, auth.ErrUnsupportedMechanism
	}

	return &auth.AuthResult{
		Identity: auth.Identity{
			Attributes: map[string]string{
				"mechanism": "kerberos",
			},
		},
		Authenticated: false,
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
