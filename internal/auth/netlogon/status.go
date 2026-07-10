package netlogon

import (
	"context"
	"errors"
	"time"
)

// ProviderKind identifies which machine-credential provider backs a running
// NETLOGON authenticator, as reported by `dfsctl netlogon status`.
type ProviderKind string

const (
	// ProviderOffline is the static, admin-supplied machine secret (hot-reloadable
	// over the API, #1325). It owns no password lifecycle, so rotation does not
	// apply — the admin rotates it by updating the machine-account config.
	ProviderOffline ProviderKind = "offline"
	// ProviderOnlineJoin provisions the computer object in AD and owns the
	// machine-password lifecycle (generate, persist, rotate), so rotation applies.
	ProviderOnlineJoin ProviderKind = "online-join"
)

// ErrRotateNotOnlineJoin is returned by Controller.Rotate for a provider that
// does not own the machine password (the offline/static provider). Rotation is
// only meaningful for the online-join provider, which generates and persists the
// password it sets on the DC; for the offline provider the admin owns the secret
// and rotates it by updating the machine-account configuration instead.
var ErrRotateNotOnlineJoin = errors.New("netlogon: machine-password rotation is only supported for the online-join provider")

// Status is a point-in-time snapshot of the NETLOGON machine-account and
// secure-channel state on the running server, surfaced by `dfsctl netlogon
// status`. It is built without directory or DC I/O.
type Status struct {
	// Provider is the active machine-credential provider ("offline" or
	// "online-join").
	Provider ProviderKind
	// AccountName is the machine-account sAMAccountName (e.g. "DITTOFS$").
	AccountName string
	// Realm is the Kerberos realm.
	Realm string
	// NetBIOSDomain is the short NetBIOS domain name.
	NetBIOSDomain string
	// DCAddresses are the configured DC addresses (empty means "discover via DNS
	// SRV from the realm").
	DCAddresses []string
	// Joined reports, for the online-join provider, whether the computer object
	// has been provisioned and its password persisted this run. It is always
	// false for the offline provider (which has no provisioning step).
	Joined bool
	// ChannelConnected reports whether a NETLOGON secure channel is currently
	// established (cached). Best-effort — see Authenticator.Connected.
	ChannelConnected bool
	// RotationEnabled reports whether automatic machine-password rotation is
	// scheduled (online-join with a positive rotation interval).
	RotationEnabled bool
	// RotationInterval is the configured automatic-rotation period (zero when
	// disabled).
	RotationInterval time.Duration
	// LastRotation is when the machine password last rotated this run; zero when
	// it has not rotated since startup.
	LastRotation time.Time
	// NextRotation is the next scheduled automatic rotation; zero when automatic
	// rotation is disabled.
	NextRotation time.Time
}

// Controller is the runtime handle for the NETLOGON machine-account subsystem.
// The wiring layer registers it with the Runtime (SetAdapterProvider "netlogon")
// so the admin API can introspect machine-account / secure-channel state and
// force a machine-password rotation on the running server — without reaching into
// the SMB adapter or the process-local authenticator.
//
// It bundles the pieces the API needs but that live in different places: the
// Authenticator (channel health), the credential provider (account identity /
// join state), and the optional RotationManager (rotation schedule).
type Controller struct {
	kind     ProviderKind
	auth     *Authenticator
	provider MachineCredentialProvider
	rot      *RotationManager // nil for offline, or online-join with rotation disabled
}

// NewController builds the runtime handle. kind must match provider's concrete
// type; rot may be nil (offline, or online-join with automatic rotation
// disabled). auth and provider must be non-nil.
func NewController(kind ProviderKind, auth *Authenticator, provider MachineCredentialProvider, rot *RotationManager) *Controller {
	return &Controller{kind: kind, auth: auth, provider: provider, rot: rot}
}

// Status returns a non-intrusive snapshot of the current machine-account and
// secure-channel state. It performs no directory or DC I/O: in particular it
// never calls the online provider's Credential (which would trigger the lazy AD
// join as a side effect), reading the provider's cached fields via snapshot
// instead.
func (c *Controller) Status(context.Context) Status {
	s := Status{Provider: c.kind}
	if c.auth != nil {
		s.ChannelConnected = c.auth.Connected()
	}
	switch p := c.provider.(type) {
	case *onlineProvider:
		snap := p.snapshot()
		s.AccountName = snap.account
		s.Realm = snap.realm
		s.NetBIOSDomain = snap.domain
		s.DCAddresses = snap.dc
		s.Joined = snap.joined
		s.LastRotation = snap.lastRotation
		if c.rot != nil {
			s.RotationEnabled = true
			s.RotationInterval = c.rot.interval
			s.NextRotation = c.rot.nextRotation()
		}
	case *MutableProvider:
		cred := p.snapshot()
		s.AccountName = cred.AccountName
		s.Realm = cred.Realm
		s.NetBIOSDomain = cred.DomainName
		s.DCAddresses = cred.DCAddresses
	}
	return s
}

// Rotate forces a machine-password rotation now. It is valid only for the
// online-join provider, which owns the password lifecycle: it runs the COMPLETE
// rotation (PasswordSet2 on the DC → switch the in-memory credential → persist),
// keeping the persisted secret in sync with the DC. Rotating the DC password
// alone (Authenticator.RotatePassword) would desync the persisted secret, so this
// deliberately routes through the provider's rotate. For the offline/static
// provider it returns ErrRotateNotOnlineJoin.
func (c *Controller) Rotate(ctx context.Context) error {
	op, ok := c.provider.(*onlineProvider)
	if !ok {
		return ErrRotateNotOnlineJoin
	}
	return op.rotate(ctx, c.auth)
}
