package netlogon

import (
	"context"
	"fmt"
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

// MachineCredentialProvider is an interface for retrieving machine credentials.
type MachineCredentialProvider interface {
	Credential(ctx context.Context) (*MachineCredential, error)
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
	if p.cred.AccountName == "" || p.cred.Password == "" || p.cred.DomainName == "" {
		return nil, fmt.Errorf("netlogon: incomplete machine credential (account/password/domain required)")
	}
	// A DC address is optional: when none is configured the secure channel
	// locates one via the AD DNS SRV record (_ldap._tcp.dc._msdcs.<realm>),
	// which requires the Kerberos realm. Require at least one of the two so we
	// never build an authenticator that can neither dial nor discover a DC (#1324).
	if len(p.cred.DCAddresses) == 0 && p.cred.Realm == "" {
		return nil, fmt.Errorf("netlogon: no DC address configured and no realm for DNS SRV discovery")
	}
	cp := p.cred
	return &cp, nil
}
