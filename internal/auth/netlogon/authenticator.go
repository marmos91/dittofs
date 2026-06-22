package netlogon

import (
	"context"
	"sync"
)

// NetworkLogonRequest carries the NTLM challenge/response the SMB handler
// received on the wire, to be validated by a Domain Controller.
type NetworkLogonRequest struct {
	Username        string
	Domain          string
	ServerChallenge [8]byte // the challenge DittoFS sent in the NTLM Type-2
	NTResponse      []byte  // client's NtChallengeResponse
	LMResponse      []byte  // client's LmChallengeResponse (may be empty)
}

// LogonResult is the identity the DC returned for a validated network logon.
type LogonResult struct {
	SessionBaseKey [16]byte
	UserSID        string
	GroupSIDs      []string
	Username       string
	DomainName     string
}

// NetlogonAuthenticator validates a domain user's NTLM response against a DC.
type NetlogonAuthenticator interface {
	NetworkLogon(ctx context.Context, req NetworkLogonRequest) (*LogonResult, error)
}

// Authenticator implements NetlogonAuthenticator via a NETLOGON sealed secure channel.
// It lazily connects to the DC on the first call and caches the channel for reuse.
// On a transient RPC error it re-establishes the channel once before giving up.
type Authenticator struct {
	provider MachineCredentialProvider
	mu       sync.Mutex
	chan_    *SecureChannel
}

// NewAuthenticator creates an Authenticator backed by the given MachineCredentialProvider.
func NewAuthenticator(p MachineCredentialProvider) *Authenticator {
	return &Authenticator{provider: p}
}

// NetworkLogon validates an NTLM network-logon response against the DC.
func (a *Authenticator) NetworkLogon(ctx context.Context, req NetworkLogonRequest) (*LogonResult, error) {
	mc, err := a.provider.Credential(ctx)
	if err != nil {
		return nil, err
	}

	sc, err := a.ensureChannel(ctx, mc)
	if err != nil {
		return nil, err
	}

	res, err := sc.samLogon(ctx, *mc, req)
	if err != nil {
		// Re-establish the channel once on RPC error, then retry.
		a.reset(ctx)
		if sc, err = a.ensureChannel(ctx, mc); err != nil {
			return nil, err
		}
		if res, err = sc.samLogon(ctx, *mc, req); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// Close shuts down the cached secure channel connection.
func (a *Authenticator) Close(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.chan_ != nil {
		a.chan_.close(ctx)
		a.chan_ = nil
	}
}

// ensureChannel returns the cached SecureChannel, creating and connecting it if needed.
func (a *Authenticator) ensureChannel(ctx context.Context, mc *MachineCredential) (*SecureChannel, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.chan_ == nil {
		a.chan_ = &SecureChannel{}
	}
	sc := a.chan_
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if err := sc.connect(ctx, *mc); err != nil {
		return nil, err
	}
	return sc, nil
}

// reset closes the cached channel and clears it so the next call re-connects.
func (a *Authenticator) reset(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.chan_ != nil {
		a.chan_.mu.Lock()
		a.chan_.close(ctx)
		a.chan_.mu.Unlock()
		a.chan_ = nil
	}
}

func samInfo4ToResult(domainSID string, userRID uint32, groupRIDs []uint32, sessionKey [16]byte, user, domain string) (*LogonResult, error) {
	userSID, err := SIDFromRID(domainSID, userRID)
	if err != nil {
		return nil, err
	}
	groups := make([]string, 0, len(groupRIDs))
	for _, rid := range groupRIDs {
		sid, err := SIDFromRID(domainSID, rid)
		if err != nil {
			return nil, err
		}
		groups = append(groups, sid)
	}
	return &LogonResult{
		SessionBaseKey: sessionKey,
		UserSID:        userSID,
		GroupSIDs:      groups,
		Username:       user,
		DomainName:     domain,
	}, nil
}
