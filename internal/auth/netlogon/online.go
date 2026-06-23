package netlogon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// OnlineConfig configures the online-join machine credential provider.
type OnlineConfig struct {
	// AccountName is the machine-account sAMAccountName (e.g. "DITTOFS$").
	AccountName string
	// Workstation is the short NetBIOS workstation name (e.g. "DITTOFS").
	Workstation string
	// DomainName is the NetBIOS domain (e.g. "DITTOFS").
	DomainName string
	// Realm is the Kerberos realm (e.g. "DITTOFS.AD").
	Realm string
	// DCAddresses are optional DC addresses for the NETLOGON secure channel;
	// when empty the DC is discovered from the realm via DNS SRV.
	DCAddresses []string
	// Join holds the LDAP join configuration (how to create the computer object).
	Join JoinConfig
	// RotationInterval is how often the machine password is rotated. Zero
	// disables automatic rotation. AD's default machine-password max age is 30
	// days; a value around 7 days is a safe default the caller may set.
	RotationInterval time.Duration
}

// onlineProvider implements MachineCredentialProvider by creating the machine
// account in AD on first use (online join) and owning the machine-password
// lifecycle: it generates the initial password, persists it, and rotates it on
// a schedule via NetrServerPasswordSet2.
//
// It is the opt-in alternative to offlineProvider. The secure channel and
// Authenticator are unchanged — they consume whatever Credential() returns.
type onlineProvider struct {
	cfg    OnlineConfig
	secret SecretStore
	dial   ldapDialer // swapped in tests; defaults to dialAndBindJoin

	mu       sync.Mutex
	password string // current machine password (loaded or generated)
	joined   bool   // true once the account is provisioned + password persisted
}

// NewOnlineProvider creates an online-join provider. The provider is lazy: no
// directory I/O happens until the first Credential() call, so construction
// cannot fail on a transient DC outage.
func NewOnlineProvider(cfg OnlineConfig, secret SecretStore) MachineCredentialProvider {
	return &onlineProvider{
		cfg:    cfg,
		secret: secret,
		dial:   dialAndBindJoin,
	}
}

// Credential returns the machine credential, performing the online join on the
// first call when no password has been persisted yet.
//
// First-call resolution:
//  1. Load the persisted password from the SecretStore. If present, the account
//     was joined on a prior run — reuse it (survives restarts).
//  2. Otherwise generate a strong password, create/reset the computer object in
//     AD (idempotent), persist the password, and use it.
func (p *onlineProvider) Credential(ctx context.Context) (*MachineCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.joined {
		if err := p.ensureJoinedLocked(ctx); err != nil {
			return nil, err
		}
	}

	if p.cfg.AccountName == "" || p.cfg.DomainName == "" || p.cfg.Realm == "" {
		return nil, fmt.Errorf("netlogon: incomplete online machine config (account/domain/realm required)")
	}

	return &MachineCredential{
		AccountName: p.cfg.AccountName,
		Password:    p.password,
		Workstation: p.cfg.Workstation,
		DomainName:  p.cfg.DomainName,
		Realm:       p.cfg.Realm,
		DCAddresses: p.cfg.DCAddresses,
	}, nil
}

// ensureJoinedLocked performs the one-time join (or reload). Must hold p.mu.
func (p *onlineProvider) ensureJoinedLocked(ctx context.Context) error {
	// 1. Reuse a previously persisted password (restart path).
	if p.secret != nil {
		stored, err := p.secret.GetMachineSecret(ctx)
		if err != nil {
			return fmt.Errorf("netlogon: load persisted machine secret: %w", err)
		}
		if stored != "" {
			p.password = stored
			p.joined = true
			slog.Default().Info("netlogon: reusing persisted machine-account password (already joined)",
				"account", p.cfg.AccountName)
			return nil
		}
	}

	// 2. First join: generate a password and create the computer object.
	newPassword, err := generateMachinePassword()
	if err != nil {
		return err
	}
	if err := joinDirectory(ctx, p.dial, &p.cfg.Join, newPassword); err != nil {
		return err
	}

	// Persist BEFORE marking joined: if persistence fails we must not treat the
	// account as joined (the DC has the new password but we'd lose it on restart).
	if p.secret != nil {
		if err := p.secret.SetMachineSecret(ctx, newPassword); err != nil {
			return fmt.Errorf("netlogon: persist machine secret after join: %w", err)
		}
	}
	p.password = newPassword
	p.joined = true
	slog.Default().Info("netlogon: online join complete; machine account provisioned and password persisted",
		"account", p.cfg.AccountName)
	return nil
}

// rotate generates a new password, sets it on the DC via the authenticator's
// established secure channel (authenticated with the CURRENT password), and on
// success persists it and switches the in-memory credential. The order matters:
// the DC must accept the new password before we persist/switch, or a crash
// between steps would leave the persisted secret out of sync with the DC.
func (p *onlineProvider) rotate(ctx context.Context, auth *Authenticator) error {
	newPassword, err := generateMachinePassword()
	if err != nil {
		return err
	}

	// PasswordSet2 over the channel authenticated with the current password.
	if err := auth.RotatePassword(ctx, newPassword); err != nil {
		return err
	}

	// The DC now holds newPassword. Update the in-memory credential FIRST so the
	// running process keeps working regardless of the persistence outcome (the
	// next channel reconnect must authenticate with the new password the DC now
	// expects — keeping the old one would break all NTLM passthrough).
	p.mu.Lock()
	p.password = newPassword
	p.mu.Unlock()

	// Then persist so the new secret survives a restart.
	if p.secret != nil {
		if err := p.secret.SetMachineSecret(ctx, newPassword); err != nil {
			// In-memory is already consistent with the DC, so the live process is
			// fine. But the persisted secret is now stale: after a restart it would
			// no longer match the DC. Surface loudly so an operator can intervene
			// (a re-join with a fresh password reconciles the account). Online join
			// itself remains the recovery path — see ensureJoinedLocked.
			slog.Default().Error("netlogon: rotated machine password set on DC but FAILED to persist; a restart before the next successful rotation will require a re-join",
				"account", p.cfg.AccountName, "error", err)
			return fmt.Errorf("netlogon: persist rotated machine secret (DC already switched): %w", err)
		}
	}

	slog.Default().Info("netlogon: machine-account password rotated", "account", p.cfg.AccountName)
	return nil
}

// RotationManager drives periodic machine-password rotation for an online
// provider. It is started by the wiring layer and stopped on shutdown.
type RotationManager struct {
	provider *onlineProvider
	auth     *Authenticator
	interval time.Duration

	stop chan struct{}
	done chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
}

// NewRotationManager wires an online provider to its authenticator for periodic
// rotation. Returns nil when prov is not an online provider or interval <= 0,
// so the caller can unconditionally call Start/Stop on the result.
func NewRotationManager(prov MachineCredentialProvider, auth *Authenticator, interval time.Duration) *RotationManager {
	op, ok := prov.(*onlineProvider)
	if !ok || interval <= 0 || auth == nil {
		return nil
	}
	return &RotationManager{
		provider: op,
		auth:     auth,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the rotation loop in a goroutine. Safe to call on a nil
// receiver (no-op) and idempotent (a second call does not spawn a second loop).
func (m *RotationManager) Start() {
	if m == nil {
		return
	}
	m.startOnce.Do(func() {
		m.started.Store(true)
		go m.run()
	})
}

func (m *RotationManager) run() {
	defer close(m.done)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := m.provider.rotate(ctx, m.auth); err != nil {
				slog.Default().Error("netlogon: machine-password rotation failed (will retry next interval)",
					"account", m.provider.cfg.AccountName, "error", err)
			}
			cancel()
		}
	}
}

// Stop halts the rotation loop and waits for the goroutine to exit. Safe on a
// nil receiver, idempotent, and non-blocking when Start was never called (no
// goroutine to wait for) — so `m := NewRotationManager(...); defer m.Stop()`
// with an early return before Start does not deadlock.
func (m *RotationManager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() { close(m.stop) })
	if !m.started.Load() {
		return
	}
	<-m.done
}
