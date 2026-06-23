package netlogon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// errChannelNotConnected is returned by a secureChannel.samLogon when the
// channel was torn down (e.g. by a concurrent Reload) before the call acquired
// the channel lock. It is a benign local race, not an RPC failure: NetworkLogon
// rebuilds and retries it (with no backoff).
var errChannelNotConnected = errors.New("netlogon: channel not connected")

// statusAccessDenied (NTSTATUS STATUS_ACCESS_DENIED) is the transient channel-level
// rejection a freshly rebuilt secure channel can return while its new MS-NRPC
// credential chain settles after a hot-reload. It is the ONLY DC-returned status
// NetworkLogon retries — every other status (a domain USER's hard auth failure
// such as LOGON_FAILURE / WRONG_PASSWORD / ACCOUNT_LOCKED_OUT) must fail fast so
// retrying never amplifies failed logons toward AD account lockout.
const statusAccessDenied uint32 = 0xC0000022

// SamLogonStatusError carries the NTSTATUS a NetrLogonSamLogon RPC returned in
// its Return field. It lets NetworkLogon distinguish a transient channel-level
// rejection (worth a rebuild+retry) from a domain user's hard authentication
// failure (must fail fast, not retry — retrying amplifies failed logons toward
// AD account lockout).
type SamLogonStatusError struct {
	Status uint32
}

func (e *SamLogonStatusError) Error() string {
	return fmt.Sprintf("netlogon: SAMLogon returned 0x%08x", e.Status)
}

// isTransientChannelError reports whether err is a transient secure-channel
// condition that a rebuild+retry can clear, as opposed to a hard logon failure.
// Only two cases qualify:
//   - errChannelNotConnected: the local teardown race (a concurrent reload tore
//     the channel down before the RPC acquired the channel lock), and
//   - a SamLogonStatusError with STATUS_ACCESS_DENIED: the channel-level rejection
//     a freshly rebuilt channel can return while its new credential chain settles.
//
// A domain user's authentication-failure NTSTATUS (LOGON_FAILURE, WRONG_PASSWORD,
// ACCOUNT_LOCKED_OUT, …) is NOT transient and returns false, so NetworkLogon fails
// fast instead of retrying a bad-password attempt toward account lockout.
func isTransientChannelError(err error) bool {
	if errors.Is(err, errChannelNotConnected) {
		return true
	}
	var st *SamLogonStatusError
	if errors.As(err, &st) {
		return st.Status == statusAccessDenied
	}
	return false
}

// maxReloadRetries bounds how many times NetworkLogon rebuilds the channel and
// retries after a transient secure-channel error (a concurrent reload tearing
// down / rebuilding the channel), so it can never live-lock. In practice a
// reload is a rare admin action and the first rebuild succeeds.
const maxReloadRetries = 8

// reloadRetryBackoff is the pause between rebuild attempts after a DC-side
// failure, giving a freshly rebuilt channel's MS-NRPC credential chain a moment
// to settle before the next attempt.
const reloadRetryBackoff = 100 * time.Millisecond

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

	// The logon failed. Only retry when the failure is a TRANSIENT secure-channel
	// condition that a rebuild can clear:
	//   - errChannelNotConnected: a concurrent Reload tore the channel down before
	//     this RPC acquired the channel lock (a benign local race), or
	//   - a SamLogonStatusError with STATUS_ACCESS_DENIED: the channel-level
	//     rejection a freshly rebuilt channel can return while its new MS-NRPC
	//     credential chain settles after a hot-reload.
	//
	// Any other failure — most importantly a domain USER's hard authentication
	// failure (LOGON_FAILURE / WRONG_PASSWORD / ACCOUNT_LOCKED_OUT, surfaced as a
	// SamLogonStatusError) — must fail FAST. Retrying a bad-password attempt would
	// amplify failed logons toward AD account lockout, so we return immediately.
	if !isTransientChannelError(err) {
		return nil, err
	}

	// Transient: rebuild against the latest credential and retry, bounded so we can
	// never live-lock, with a short backoff to let the rebuilt channel's credential
	// chain settle.
	//
	// A credential-provider error is NOT retried: when passthrough is disabled the
	// provider returns a validation error, so the loop returns immediately and the
	// logon fails closed (it never silently keeps authenticating with a stale
	// credential).
	for attempt := 0; attempt < maxReloadRetries; attempt++ {
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
		// A non-transient failure on the rebuilt channel (e.g. the user's password
		// really is wrong) must fail fast too — stop retrying immediately.
		if !isTransientChannelError(err) {
			return nil, err
		}
		// Brief backoff before the next rebuild so a just-rebuilt channel is not
		// immediately torn down again by a still-in-flight reload. Skipped for the
		// purely-local "channel not connected" race, which needs no settle time.
		if !errors.Is(err, errChannelNotConnected) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(reloadRetryBackoff):
			}
		}
	}
	return nil, err
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
