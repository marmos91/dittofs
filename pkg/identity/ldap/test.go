package ldap

import (
	"context"
	"encoding/json"
	"fmt"
)

// MarshalStored serializes a Config for trusted server-side persistence,
// INCLUDING the bind password. It deliberately bypasses Config.MarshalJSON
// (which redacts the password) so the persisted configuration is usable after
// a restart. Never use this for anything client-facing — use the redacting
// Config.MarshalJSON / the API DTO layer for that.
func MarshalStored(cfg *Config) ([]byte, error) {
	type alias Config
	return json.Marshal((*alias)(cfg))
}

// UnmarshalStored is the inverse of MarshalStored: it decodes a stored config
// blob (with the bind password intact) back into a Config.
func UnmarshalStored(data []byte) (*Config, error) {
	type alias Config
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	c := Config(a)
	return &c, nil
}

// TestConnection performs a dry-run reachability check against the directory
// described by cfg: it applies defaults, validates, dials, and binds as the
// service account, then closes the connection. Nothing is persisted and no
// user is resolved.
//
// It backs the control-plane "test identity provider" endpoint. Returned
// errors describe the failing stage (validation, dial, or bind) and never
// include the bind password — the underlying go-ldap bind error reports only
// the LDAP result code, not the credential.
func TestConnection(ctx context.Context, cfg *Config) error {
	return testConnection(ctx, cfg, dialAndBind)
}

// testConnection is the connect-injectable core of TestConnection so the
// dial+bind path can be exercised in unit tests with a fake connection.
func testConnection(ctx context.Context, cfg *Config, connect connectFunc) error {
	if cfg == nil {
		return fmt.Errorf("ldap: nil config")
	}
	// Copy so ApplyDefaults/Validate never mutate the caller's config (the
	// handler reuses the same struct to persist after a successful test).
	c := *cfg
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return err
	}
	conn, err := connect(ctx, &c)
	if err != nil {
		return fmt.Errorf("ldap: connect/bind: %w", err)
	}
	return conn.Close()
}
