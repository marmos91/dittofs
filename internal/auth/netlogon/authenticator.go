package netlogon

import (
	"context"
	"errors"
	"sync"
)

// errChannelNotConnected is returned by a secureChannel.samLogon when the
// channel was torn down (e.g. by a concurrent Reload) before the call acquired
// the channel lock. It is a benign race, not an RPC failure: NetworkLogon
// rebuilds and retries it without consuming the single RPC-error retry.
var errChannelNotConnected = errors.New("netlogon: channel not connected")

// maxReloadRetries bounds how many times NetworkLogon rebuilds the channel after
// hitting errChannelNotConnected, so a pathological reload storm cannot live-lock
// a logon. In practice a reload is a rare admin action and one rebuild suffices.
const maxReloadRetries = 8

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

// secureChannel is the subset of a NETLOGON secure channel the Authenticator
// depends on. *SecureChannel is the production implementation; tests substitute
// a fake to exercise the teardown/concurrency logic without real RPC.
//
// connect, samLogon, and close each take the channel's own lock for their full
// duration, so a teardown (close) cannot race an in-flight NetrLogonSamLogon and
// corrupt the MS-NRPC sequence number chained across calls.
type secureChannel interface {
	connect(ctx context.Context, mc MachineCredential) error
	samLogon(ctx context.Context, mc MachineCredential, req NetworkLogonRequest) (*LogonResult, error)
	close(ctx context.Context)
}

// newSecureChannel is the channel factory; a package var so tests can inject a
// fake. Production always builds a real *SecureChannel.
var newSecureChannel = func() secureChannel { return &SecureChannel{} }

// Authenticator implements NetlogonAuthenticator via a NETLOGON sealed secure channel.
// It lazily connects to the DC on the first call and caches the channel for reuse.
// On a transient RPC error it re-establishes the channel once before giving up.
//
// The cached channel pointer is swapped atomically under a.mu by
// ensureChannel/reset, and every channel operation locks the channel itself for
// its full duration. A Reload (machine-credential hot-reload, #1325) therefore
// tears the channel down without racing an in-flight NetrLogonSamLogon.
type Authenticator struct {
	provider MachineCredentialProvider
	mu       sync.Mutex
	chan_    secureChannel
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
	if err == nil {
		return res, nil
	}

	// A concurrent Reload that tore sc down between ensureChannel and samLogon
	// surfaces as errChannelNotConnected — a benign race, not an RPC failure.
	// Rebuild against the latest credential and retry, bounded so a reload storm
	// cannot live-lock. Any other error gets a single RPC-error rebuild+retry.
	retries := maxReloadRetries
	for {
		a.reset(ctx)
		mc, err = a.provider.Credential(ctx)
		if err != nil {
			return nil, err
		}
		if sc, err = a.ensureChannel(ctx, mc); err != nil {
			return nil, err
		}
		res, err = sc.samLogon(ctx, *mc, req)
		if err == nil {
			return res, nil
		}
		// Only the reload race is retried beyond the first attempt; a genuine RPC
		// error is returned after one rebuild+retry.
		if !errors.Is(err, errChannelNotConnected) {
			return nil, err
		}
		retries--
		if retries <= 0 {
			return nil, err
		}
	}
}

// Reload tears down the cached channel so the next NetworkLogon rebuilds it
// against the current machine credential.
//
// Safe under concurrent logons: the old channel is closed under its own lock, so
// the teardown blocks until any in-flight NetrLogonSamLogon completes and never
// corrupts the chained sequence number. In-flight logons finish on the old
// channel; the next logon builds a fresh one with the new credential.
func (a *Authenticator) Reload(ctx context.Context) {
	a.reset(ctx)
}

// ReloadCredential is the NETLOGON machine-credential hot-reload entrypoint
// (#1325). When the authenticator is backed by a *MutableProvider, it installs
// the new credential and then tears down the cached channel atomically so the
// next NetworkLogon authenticates with the new credential / DC binding — without
// a server restart. With any other provider it falls back to dropping the cached
// channel (the provider re-supplies the credential on the next call).
func (a *Authenticator) ReloadCredential(ctx context.Context, cred MachineCredential) {
	if mp, ok := a.provider.(*MutableProvider); ok {
		mp.Set(cred)
	}
	a.reset(ctx)
}

// Close shuts down the cached secure channel connection.
func (a *Authenticator) Close(ctx context.Context) {
	a.reset(ctx)
}

// ensureChannel returns the cached secureChannel, creating and connecting it if
// needed. connect is idempotent and self-locking; a concurrent reset that clears
// a.chan_ just causes the next call to build a fresh channel.
func (a *Authenticator) ensureChannel(ctx context.Context, mc *MachineCredential) (secureChannel, error) {
	a.mu.Lock()
	if a.chan_ == nil {
		a.chan_ = newSecureChannel()
	}
	sc := a.chan_
	a.mu.Unlock()

	if err := sc.connect(ctx, *mc); err != nil {
		// Drop the half-built channel so the next attempt starts clean, but only
		// if it is still the cached one (a concurrent reset may have replaced it).
		a.mu.Lock()
		if a.chan_ == sc {
			a.chan_ = nil
		}
		a.mu.Unlock()
		return nil, err
	}
	return sc, nil
}

// reset detaches and closes the cached channel so the next call re-connects.
// The pointer swap happens under a.mu; close runs after a.mu is released (taking
// only the channel's own lock) so a slow teardown does not block new logons from
// installing a fresh channel.
func (a *Authenticator) reset(ctx context.Context) {
	a.mu.Lock()
	old := a.chan_
	a.chan_ = nil
	a.mu.Unlock()
	if old != nil {
		old.close(ctx)
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
