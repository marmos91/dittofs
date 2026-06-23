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
	// The realm is mandatory regardless of how the DC is located: the secure
	// channel rides a Kerberos-authenticated SMB session (buildKRB5Config needs
	// the realm) and, when no DC address is configured, the realm also drives
	// DNS SRV discovery. A DC address itself is optional — absent one, the secure
	// channel locates a DC from the realm via _ldap._tcp.dc._msdcs.<realm> (#1324).
	if p.cred.Realm == "" {
		return nil, fmt.Errorf("netlogon: realm is required (Kerberos SMB session to the DC and DNS SRV discovery both need it)")
	}
	cp := p.cred
	return &cp, nil
}
