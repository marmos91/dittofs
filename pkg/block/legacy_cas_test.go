package block

import (
	"errors"
	"strings"
	"testing"
)

// Tests for the migration-only legacy cas/ key helpers (legacy_cas.go).
// Deleted with that file when the cas→blocks migration is retired.

// TestFormatCASKey asserts FormatCASKey returns exactly
// "cas/{hex[0:2]}/{hex[2:4]}/{hex}" for both the all-zero hash and a
// known-vector hash.
func TestFormatCASKey(t *testing.T) {
	tests := []struct {
		name string
		hash func() ContentHash
		want string
	}{
		{
			name: "all-zero hash",
			hash: func() ContentHash { return ContentHash{} },
			want: "cas/00/00/0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name: "known vector (blake3 of empty input)",
			hash: func() ContentHash {
				h, err := ParseContentHash(blake3EmptyHex)
				if err != nil {
					t.Fatalf("setup: ParseContentHash(%q) error: %v", blake3EmptyHex, err)
				}
				return h
			},
			want: "cas/af/13/" + blake3EmptyHex,
		},
		{
			name: "incrementing-byte hash",
			hash: func() ContentHash {
				var h ContentHash
				for i := range h {
					h[i] = byte(i)
				}
				return h
			},
			want: "cas/00/01/000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatCASKey(tt.hash())
			if got != tt.want {
				t.Fatalf("FormatCASKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseCASKey_RoundTrip asserts ParseCASKey accepts the output of
// FormatCASKey and returns the original hash unchanged.
func TestParseCASKey_RoundTrip(t *testing.T) {
	hashes := []func() ContentHash{
		func() ContentHash { return ContentHash{} },
		func() ContentHash {
			h, _ := ParseContentHash(blake3EmptyHex)
			return h
		},
		func() ContentHash {
			var h ContentHash
			for i := range h {
				h[i] = byte(0xAA)
			}
			return h
		},
	}
	for i, mk := range hashes {
		h := mk()
		key := FormatCASKey(h)
		got, err := ParseCASKey(key)
		if err != nil {
			t.Fatalf("case %d: ParseCASKey(%q) error: %v", i, key, err)
		}
		if got != h {
			t.Fatalf("case %d: ParseCASKey round-trip mismatch:\n got: %x\nwant: %x", i, got, h)
		}
	}
}

// TestParseCASKey_Malformed asserts ParseCASKey rejects malformed inputs
// with ErrCASKeyMalformed wrapped via fmt.Errorf %w.
func TestParseCASKey_Malformed(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "empty string", key: ""},
		{name: "missing prefix", key: "blake3/00/00/" + blake3EmptyHex},
		{name: "wrong prefix", key: "chunk/af/13/" + blake3EmptyHex},
		{name: "shard1 too short", key: "cas/a/13/" + blake3EmptyHex},
		{name: "shard1 too long", key: "cas/aff/13/" + blake3EmptyHex},
		{name: "shard2 too short", key: "cas/af/1/" + blake3EmptyHex},
		{name: "missing third segment", key: "cas/af/13"},
		{name: "extra trailing segment", key: "cas/af/13/" + blake3EmptyHex + "/extra"},
		{name: "odd-length hex", key: "cas/af/13/" + blake3EmptyHex + "0"},
		{name: "non-hex chars", key: "cas/zz/13/" + strings.Repeat("z", 64)},
		{name: "shard does not match hash prefix", key: "cas/00/00/" + blake3EmptyHex},
		{name: "payload-style key", key: "export/file.txt/block-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCASKey(tt.key)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tt.key)
			}
			if !errors.Is(err, ErrCASKeyMalformed) {
				t.Fatalf("ParseCASKey(%q) error = %v, want errors.Is(err, ErrCASKeyMalformed)", tt.key, err)
			}
		})
	}
}
