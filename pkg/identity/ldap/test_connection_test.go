package ldap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTestConnection_DialsAndBinds(t *testing.T) {
	called := false
	connect := func(context.Context, *Config) (conn, error) {
		called = true
		return &fakeConn{}, nil
	}
	if err := testConnection(context.Background(), baseCfg(), connect); err != nil {
		t.Fatalf("testConnection: %v", err)
	}
	if !called {
		t.Fatal("expected connect to be called")
	}
}

func TestTestConnection_ValidationFailsBeforeDial(t *testing.T) {
	cfg := baseCfg()
	cfg.URL = "" // enabled but no URL → Validate must reject
	called := false
	connect := func(context.Context, *Config) (conn, error) {
		called = true
		return &fakeConn{}, nil
	}
	if err := testConnection(context.Background(), cfg, connect); err == nil {
		t.Fatal("expected validation error")
	}
	if called {
		t.Fatal("connect must not be called when validation fails")
	}
}

func TestTestConnection_ConnectError(t *testing.T) {
	wantErr := errors.New("dial refused")
	connect := func(context.Context, *Config) (conn, error) { return nil, wantErr }
	err := testConnection(context.Background(), baseCfg(), connect)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped connect error, got %v", err)
	}
}

func TestTestConnection_DoesNotMutateCaller(t *testing.T) {
	cfg := baseCfg()
	cfg.UserAttr = "" // ApplyDefaults would set this on a copy, not the caller
	connect := func(context.Context, *Config) (conn, error) { return &fakeConn{}, nil }
	if err := testConnection(context.Background(), cfg, connect); err != nil {
		t.Fatalf("testConnection: %v", err)
	}
	if cfg.UserAttr != "" {
		t.Errorf("caller config mutated: UserAttr = %q", cfg.UserAttr)
	}
}

func TestTestConnection_NilConfig(t *testing.T) {
	if err := TestConnection(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestMarshalStored_PreservesPasswordRoundTrip(t *testing.T) {
	cfg := baseCfg()
	cfg.BindPassword = "s3cret"

	data, err := MarshalStored(cfg)
	if err != nil {
		t.Fatalf("MarshalStored: %v", err)
	}
	if !strings.Contains(string(data), "s3cret") {
		t.Fatalf("stored blob must contain the real password, got %s", data)
	}

	got, err := UnmarshalStored(data)
	if err != nil {
		t.Fatalf("UnmarshalStored: %v", err)
	}
	if got.BindPassword != "s3cret" || got.URL != cfg.URL || got.BaseDN != cfg.BaseDN {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// The redacting marshaler must NOT leak the password (contrast with stored).
	redacted, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(redacted), "s3cret") {
		t.Fatalf("redacting marshaler leaked password: %s", redacted)
	}
}
