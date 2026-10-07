package keyprovider

import (
	"context"
	"errors"
	"testing"
)

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
