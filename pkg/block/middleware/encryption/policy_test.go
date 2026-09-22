package encryption

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParsePolicy_Defaults(t *testing.T) {
	policy, err := ParsePolicy(json.RawMessage(`{"key":{"kind":"local","file":"/etc/dittofs/share.key"}}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if policy.AEAD != AEADAES256GCM {
		t.Errorf("AEAD default: got %v want %v", policy.AEAD, AEADAES256GCM)
	}
	if policy.Key.Kind != "local" {
		t.Errorf("Key.Kind: got %q want %q", policy.Key.Kind, "local")
	}
	if policy.Key.File != "/etc/dittofs/share.key" {
		t.Errorf("Key.File: got %q", policy.Key.File)
	}
}

func TestParsePolicy_AllAEADs(t *testing.T) {
	cases := []struct {
		in   string
		want AEAD
	}{
		{`{"aead":"aes-256-gcm","key":{"kind":"local"}}`, AEADAES256GCM},
		{`{"aead":"chacha20-poly1305","key":{"kind":"local"}}`, AEADChaCha20Poly1305},
		{`{"aead":"xchacha20-poly1305","key":{"kind":"local"}}`, AEADXChaCha20Poly1305},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p, err := ParsePolicy(json.RawMessage(tc.in))
			if err != nil {
				t.Fatalf("ParsePolicy: %v", err)
			}
			if p.AEAD != tc.want {
				t.Errorf("AEAD: got %v want %v", p.AEAD, tc.want)
			}
		})
	}
}

func TestParsePolicy_UnknownAEAD(t *testing.T) {
	_, err := ParsePolicy(json.RawMessage(`{"aead":"twofish","key":{"kind":"local"}}`))
	if !errors.Is(err, ErrUnsupportedAEAD) {
		t.Fatalf("unknown AEAD: want ErrUnsupportedAEAD, got %v", err)
	}
}

func TestParsePolicy_RejectsNonObject(t *testing.T) {
	cases := []string{`null`, `[]`, `"aes"`, `42`, ``}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParsePolicy(json.RawMessage(in))
			if err == nil {
				t.Fatalf("want rejection for %q", in)
			}
		})
	}
}

func TestParsePolicy_BadJSON(t *testing.T) {
	_, err := ParsePolicy(json.RawMessage(`{"aead":"aes-256-gcm",`))
	if err == nil {
		t.Fatal("malformed JSON should error")
	}
}

func TestAEAD_String(t *testing.T) {
	if AEADAES256GCM.String() != "aes-256-gcm" {
		t.Error("AES-256-GCM string mismatch")
	}
	if AEAD(99).String() == "" {
		t.Error("unknown AEAD String() returned empty")
	}
}
