package keyprovider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
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
