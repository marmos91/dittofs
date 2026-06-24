package netlogon

import "context"

// SecretStore persists the machine-account password across restarts for the
// online-join provider. It is a deliberately tiny surface (a scoped key/value
// pair) so the netlogon package never imports the control-plane store directly;
// cmd/dfs adapts the GORM-backed SettingsStore to this interface.
//
// The stored value is the machine-account password in cleartext. It is no more
// sensitive than the offline provider's config-supplied secret (which lives in
// the same DB / config), and it MUST round-trip byte-for-byte: NETLOGON derives
// the session key from it, so any transformation would break the secure channel.
type SecretStore interface {
	// GetMachineSecret returns the persisted machine password, or "" (no error)
	// when none has been stored yet (first boot before a join).
	GetMachineSecret(ctx context.Context) (string, error)
	// SetMachineSecret persists the machine password, overwriting any prior value.
	SetMachineSecret(ctx context.Context, secret string) error
}
