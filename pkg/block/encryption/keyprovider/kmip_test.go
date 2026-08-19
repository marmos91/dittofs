package keyprovider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gemalto/kmip-go/kmip14"
)

// requireKMIPEnv gates KMIP integration tests behind DITTOFS_TEST_KMIP=1
// plus a set of endpoint / credential paths. Mirrors the gating pattern
// used by the S3 tests under test/e2e/run-e2e.sh --s3.
func requireKMIPEnv(t *testing.T) Config {
	t.Helper()
	if os.Getenv("DITTOFS_TEST_KMIP") != "1" {
		t.Skip("DITTOFS_TEST_KMIP=1 required for KMIP integration tests")
	}
	endpoint := os.Getenv("DITTOFS_TEST_KMIP_ENDPOINT")
	cert := os.Getenv("DITTOFS_TEST_KMIP_CERT")
	key := os.Getenv("DITTOFS_TEST_KMIP_KEY")
	ca := os.Getenv("DITTOFS_TEST_KMIP_CA")
	uid := os.Getenv("DITTOFS_TEST_KMIP_KEY_UID")
	if endpoint == "" || cert == "" || key == "" || uid == "" {
		t.Skip("DITTOFS_TEST_KMIP_{ENDPOINT,CERT,KEY,KEY_UID} all required")
	}
	return Config{
		Kind:       KindKMIP,
		Endpoint:   endpoint,
		ClientCert: cert,
		ClientKey:  key,
		ServerCA:   ca,
		KeyUID:     uid,
	}
}

func TestKMIP_WrapUnwrapRoundTrip(t *testing.T) {
	cfg := requireKMIPEnv(t)
	p, err := newKMIPProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newKMIPProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	blockKey := bytes.Repeat([]byte{0x55}, 32)
	wrapped, id, err := p.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	got, err := p.Unwrap(context.Background(), wrapped, id)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(got, blockKey) {
		t.Fatalf("Unwrap returned %x, want %x", got, blockKey)
	}
}

// kmipStateUID returns the uid of a key the live server holds in a
// particular state, or skips when the harness did not provision one. The
// states come from test/kmip/provision.py.
func kmipStateUID(t *testing.T, envVar string) string {
	t.Helper()
	uid := os.Getenv(envVar)
	if uid == "" {
		t.Skipf("%s required", envVar)
	}
	return uid
}

// TestKMIP_RetiredUIDStillUnwrapsLive is the rotation property against a
// real server: a block wrapped under one uid still unwraps after the
// current uid has moved on and the first is listed as retired.
func TestKMIP_RetiredUIDStillUnwrapsLive(t *testing.T) {
	cfg := requireKMIPEnv(t)
	newUID := kmipStateUID(t, "DITTOFS_TEST_KMIP_RETIRED_KEY_UID")

	before, err := newKMIPProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newKMIPProvider(pre-rotation): %v", err)
	}
	blockKey := bytes.Repeat([]byte{0x44}, 32)
	wrapped, oldID, err := before.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	_ = before.Close()

	rotated := cfg
	rotated.KeyUID = newUID
	rotated.RetiredKeyUIDs = []string{cfg.KeyUID}
	after, err := newKMIPProvider(context.Background(), rotated)
	if err != nil {
		t.Fatalf("newKMIPProvider(rotated): %v", err)
	}
	t.Cleanup(func() { _ = after.Close() })

	if after.CurrentMasterKeyID() != newUID {
		t.Fatalf("current master key id = %q, want %q", after.CurrentMasterKeyID(), newUID)
	}
	got, err := after.Unwrap(context.Background(), wrapped, oldID)
	if err != nil {
		t.Fatalf("Unwrap of pre-rotation block: %v", err)
	}
	if !bytes.Equal(got, blockKey) {
		t.Fatalf("Unwrap returned %x, want %x", got, blockKey)
	}
}

// TestKMIP_CurrentKeyStateRefusedLive pins the refusal against a real
// server: a current key the HSM has moved out of Active keeps the
// encrypted remote from coming up, and the error names both the state and
// the uid so an operator can act on it.
func TestKMIP_CurrentKeyStateRefusedLive(t *testing.T) {
	base := requireKMIPEnv(t)
	for _, tc := range []struct {
		envVar string
		state  kmip14.State
	}{
		{"DITTOFS_TEST_KMIP_DEACTIVATED_KEY_UID", kmip14.StateDeactivated},
		{"DITTOFS_TEST_KMIP_COMPROMISED_KEY_UID", kmip14.StateCompromised},
		{"DITTOFS_TEST_KMIP_PREACTIVE_KEY_UID", kmip14.StatePreActive},
	} {
		t.Run(tc.state.String(), func(t *testing.T) {
			cfg := base
			cfg.KeyUID = kmipStateUID(t, tc.envVar)
			p, err := newKMIPProvider(context.Background(), cfg)
			if err == nil {
				_ = p.Close()
				t.Fatalf("newKMIPProvider accepted a %s current key", tc.state)
			}
			if !errors.Is(err, ErrKeyStateUnusable) {
				t.Fatalf("error = %v, want ErrKeyStateUnusable", err)
			}
			if !strings.Contains(err.Error(), tc.state.String()) || !strings.Contains(err.Error(), cfg.KeyUID) {
				t.Fatalf("error %q should name both the state %q and the uid %q", err, tc.state, cfg.KeyUID)
			}
		})
	}
}

// TestKMIP_RetiredCompromisedKeyLoadsLive covers the other half of the
// state policy: a retired key the HSM marks Compromised is still loaded,
// because the blocks written under it have to stay readable.
func TestKMIP_RetiredCompromisedKeyLoadsLive(t *testing.T) {
	cfg := requireKMIPEnv(t)
	compromised := kmipStateUID(t, "DITTOFS_TEST_KMIP_COMPROMISED_KEY_UID")
	cfg.RetiredKeyUIDs = []string{compromised}

	p, err := newKMIPProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newKMIPProvider with a compromised retired uid: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if _, ok := p.retired[compromised]; !ok {
		t.Fatalf("retired set does not hold %q; a compromised retired key must stay readable", compromised)
	}
}

// TestKMIP_ConfigValidation runs without a live KMIP server — it
// exercises the up-front config checks in newKMIPProvider that have to
// fail fast before any network I/O.
func TestKMIP_ConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing endpoint", Config{Kind: KindKMIP, KeyUID: "x", ClientCert: "/a", ClientKey: "/b"}},
		{"missing key_uid", Config{Kind: KindKMIP, Endpoint: "host:5696", ClientCert: "/a", ClientKey: "/b"}},
		{"missing client cert", Config{Kind: KindKMIP, Endpoint: "host:5696", KeyUID: "x", ClientKey: "/b"}},
		{"missing client key", Config{Kind: KindKMIP, Endpoint: "host:5696", KeyUID: "x", ClientCert: "/a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newKMIPProvider(context.Background(), tc.cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
		})
	}
}
