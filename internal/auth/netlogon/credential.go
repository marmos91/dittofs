package netlogon

import (
	"context"
	"fmt"
)

// MachineCredential holds the machine account credentials required for NETLOGON operations.
type MachineCredential struct {
	AccountName  string   // Machine account name (e.g., "DITTOFS$")
	Password     string   // Machine account password
	Workstation  string   // Workstation name
	DomainName   string   // Domain name (NetBIOS)
	Realm        string   // Realm (Kerberos)
	DCAddresses  []string // Domain controller addresses
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
	if p.cred.AccountName == "" || p.cred.Password == "" ||
		p.cred.Workstation == "" || p.cred.DomainName == "" ||
		p.cred.Realm == "" || len(p.cred.DCAddresses) == 0 {
		return nil, fmt.Errorf("incomplete machine credential: all fields required")
	}
	return &p.cred, nil
}
