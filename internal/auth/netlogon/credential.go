package netlogon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// MachineCredential holds the machine account credentials required for NETLOGON operations.
type MachineCredential struct {
	AccountName string   // Machine account name (e.g., "DITTOFS$")
	Password    string   // Machine account password
	Workstation string   // Workstation name
	DomainName  string   // Domain name (NetBIOS)
	Realm       string   // Realm (Kerberos)
	DCAddresses []string // Domain controller addresses
}

// DeriveWorkstation derives the short NetBIOS workstation name used in NETLOGON
// RPC calls (MS-NRPC §3.1.4.1 requires the short host name without the trailing
// '$' machine-account marker). Priority:
//  1. accountName with a trailing '$' stripped (the AD convention, e.g.
//     "DITTOFS$" → "DITTOFS").
//  2. Short hostname from os.Hostname() (everything before the first '.').
func DeriveWorkstation(accountName string) string {
	if accountName != "" {
		return strings.TrimSuffix(accountName, "$")
	}
	if h, err := os.Hostname(); err == nil {
		if idx := strings.IndexByte(h, '.'); idx > 0 {
			return h[:idx]
		}
		return h
	}
	slog.Warn("NETLOGON workstation name could not be derived; DC may reject the logon")
	return ""
}

// BuildMachineCredential assembles a MachineCredential from the machine-account
// configuration fields, deriving the workstation name via DeriveWorkstation. It
// is shared by the startup wiring (cmd/dfs) and the API hot-reload path so both
// produce identical credentials (#1325). It performs no validation — callers
// rely on Credential() / validateCredential to reject incomplete credentials.
func BuildMachineCredential(accountName, secret, netbiosDomain, realm string, dcAddresses []string) MachineCredential {
	return MachineCredential{
		AccountName: accountName,
		Password:    secret,
		Workstation: DeriveWorkstation(accountName),
		DomainName:  netbiosDomain,
		Realm:       realm,
		DCAddresses: dcAddresses,
	}
}

// MachineCredentialProvider is an interface for retrieving machine credentials.
type MachineCredentialProvider interface {
	Credential(ctx context.Context) (*MachineCredential, error)
}

// validateCredential checks that a MachineCredential carries the fields the
// secure channel needs: account/password/domain are always required, and the
// realm is mandatory (the channel rides a Kerberos SMB session and the realm
// also drives DNS SRV DC discovery when no DC address is configured, #1324).
func validateCredential(c MachineCredential) error {
	if c.AccountName == "" || c.Password == "" || c.DomainName == "" {
		return fmt.Errorf("netlogon: incomplete machine credential (account/password/domain required)")
	}
	if c.Realm == "" {
		return fmt.Errorf("netlogon: realm is required (Kerberos SMB session to the DC and DNS SRV discovery both need it)")
	}
	return nil
}

// MutableProvider is a MachineCredentialProvider whose credential can be swapped
// atomically at runtime. It backs NETLOGON machine-credential hot-reload (#1325):
// an API-driven machine-account config change calls Set to install the new
// credential, after which the next secure-channel rebuild authenticates with it.
// Concurrent Credential reads and Set writes are mutex-guarded.
type MutableProvider struct {
	mu   sync.RWMutex
	cred MachineCredential
}

// NewMutableProvider creates a MutableProvider seeded with the given credential.
func NewMutableProvider(cred MachineCredential) *MutableProvider {
	return &MutableProvider{cred: cred}
}

// Credential returns a copy of the current credential after validation.
func (p *MutableProvider) Credential(ctx context.Context) (*MachineCredential, error) {
	p.mu.RLock()
	cp := p.cred
	p.mu.RUnlock()
	if err := validateCredential(cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// snapshot returns a copy of the current credential WITHOUT validation, for
// status introspection: `netlogon status` must report the account/realm/domain
// even when the credential is incomplete (e.g. after an API hot-reload disabled
// passthrough by installing an empty credential), where Credential would instead
// return a validation error.
func (p *MutableProvider) snapshot() MachineCredential {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cred
}

// Set atomically replaces the credential. The next Credential call (and thus the
// next secure-channel rebuild) uses the new value. It does not itself tear down
// any cached channel — callers that want the change to take effect immediately
// must reset the Authenticator's channel (see Authenticator.Reload).
func (p *MutableProvider) Set(cred MachineCredential) {
	p.mu.Lock()
	p.cred = cred
	p.mu.Unlock()
}

// offlineProvider implements MachineCredentialProvider with a static credential.
type offlineProvider struct {
	cred MachineCredential
}

// NewOfflineProvider creates a new offline provider with the given credential.
// It validates that all required fields are present.
func NewOfflineProvider(cred MachineCredential) MachineCredentialProvider {
	return &offlineProvider{cred: cred}
}

// Credential returns the stored credential after validation.
func (p *offlineProvider) Credential(ctx context.Context) (*MachineCredential, error) {
	if err := validateCredential(p.cred); err != nil {
		return nil, err
	}
	cp := p.cred
	return &cp, nil
}
