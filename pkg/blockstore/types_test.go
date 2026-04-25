package blockstore

import (
	"strings"
	"testing"
)

// TestContentHash_CASKey_Format is the FIX-9 explicit format guard:
// asserts the prefix, total length, and exact hex serialization of CASKey
// for a deterministic hash pattern. This sits alongside TestContentHashCASKey
// (which only asserts the exact string) so accidental changes to the prefix
// or hex width fail loudly.
func TestContentHash_CASKey_Format(t *testing.T) {
	var h ContentHash
	for i := range h {
		h[i] = byte(i) // 00 01 02 ... 1F
	}
	got := h.CASKey()
	want := "blake3:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if got != want {
		t.Fatalf("CASKey() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "blake3:") {
		t.Fatalf("CASKey() lacks blake3: prefix: %q", got)
	}
	if len(got) != len("blake3:")+64 {
		t.Fatalf("CASKey() len = %d, want %d", len(got), len("blake3:")+64)
	}
}

// TestContentHashCASKey asserts CASKey returns the "blake3:{hex}" scheme
// for a known hash pattern. Phase 10 ships the helper ahead of the Phase 11
// CAS write-path wiring (D-06).
func TestContentHashCASKey(t *testing.T) {
	var h ContentHash
	for i := 0; i < HashSize; i++ {
		h[i] = byte(i)
	}
	got := h.CASKey()
	want := "blake3:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if got != want {
		t.Fatalf("CASKey mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestContentHashCASKey_ZeroValue covers the uninitialized ContentHash path
// — the zero value must still produce a well-formed CAS key.
func TestContentHashCASKey_ZeroValue(t *testing.T) {
	var h ContentHash
	got := h.CASKey()
	want := "blake3:0000000000000000000000000000000000000000000000000000000000000000"
	if got != want {
		t.Fatalf("CASKey zero value: got %q want %q", got, want)
	}
}

// TestContentHashString_Unchanged locks in the invariant that CASKey and
// String differ only by the "blake3:" prefix — ensures legacy String()
// callers are not disturbed by the CASKey addition.
func TestContentHashString_Unchanged(t *testing.T) {
	var h ContentHash
	for i := 0; i < HashSize; i++ {
		h[i] = byte(0xAA)
	}
	wantHex := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if got := h.String(); got != wantHex {
		t.Fatalf("String: got %q want %q", got, wantHex)
	}
	if got := h.CASKey(); got != "blake3:"+h.String() {
		t.Fatalf("CASKey should equal \"blake3:\" + String(); got %q", got)
	}
}

// TestParseStoreKey_RoundTrip covers the canonical external store-key parser
// (format: "{payloadID}/block-{N}"). Added here for symmetry with ParseBlockID
// tests as part of TD-04 (5 parsers -> 2).
func TestParseStoreKey_RoundTrip(t *testing.T) {
	tests := []struct {
		name          string
		storeKey      string
		wantPayloadID string
		wantBlockIdx  uint64
		wantOK        bool
	}{
		{
			name:          "simple key",
			storeKey:      "export/file.txt/block-0",
			wantPayloadID: "export/file.txt",
			wantBlockIdx:  0,
			wantOK:        true,
		},
		{
			name:          "nested path",
			storeKey:      "export/docs/report.pdf/block-7",
			wantPayloadID: "export/docs/report.pdf",
			wantBlockIdx:  7,
			wantOK:        true,
		},
		{
			name:          "high block index",
			storeKey:      "export/large.bin/block-12345",
			wantPayloadID: "export/large.bin",
			wantBlockIdx:  12345,
			wantOK:        true,
		},
		{
			name:     "missing /block- marker",
			storeKey: "export/file.txt",
			wantOK:   false,
		},
		{
			name:     "non-integer index",
			storeKey: "export/file.txt/block-abc",
			wantOK:   false,
		},
		{
			name:     "empty string",
			storeKey: "",
			wantOK:   false,
		},
		{
			name:     "marker at index 0",
			storeKey: "/block-0",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, idx, ok := ParseStoreKey(tt.storeKey)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if pid != tt.wantPayloadID {
				t.Errorf("payloadID = %q, want %q", pid, tt.wantPayloadID)
			}
			if idx != tt.wantBlockIdx {
				t.Errorf("blockIdx = %d, want %d", idx, tt.wantBlockIdx)
			}
		})
	}
}

// TestParseBlockID_RoundTrip covers the canonical internal blockID parser
// (format: "{payloadID}/{blockIdx}"). Part of TD-04 consolidation (5 -> 2).
func TestParseBlockID_RoundTrip(t *testing.T) {
	tests := []struct {
		name          string
		blockID       string
		wantPayloadID string
		wantBlockIdx  uint64
	}{
		{
			name:          "nested payload with numeric idx",
			blockID:       "export/docs/report.pdf/7",
			wantPayloadID: "export/docs/report.pdf",
			wantBlockIdx:  7,
		},
		{
			name:          "simple payload with idx 0",
			blockID:       "export/file.txt/0",
			wantPayloadID: "export/file.txt",
			wantBlockIdx:  0,
		},
		{
			name:          "payload with multiple slashes splits on LAST /",
			blockID:       "a/b/c/d/42",
			wantPayloadID: "a/b/c/d",
			wantBlockIdx:  42,
		},
		{
			name:          "high block index",
			blockID:       "share/big.bin/9999999",
			wantPayloadID: "share/big.bin",
			wantBlockIdx:  9999999,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, idx, err := ParseBlockID(tt.blockID)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pid != tt.wantPayloadID {
				t.Errorf("payloadID = %q, want %q", pid, tt.wantPayloadID)
			}
			if idx != tt.wantBlockIdx {
				t.Errorf("blockIdx = %d, want %d", idx, tt.wantBlockIdx)
			}
		})
	}
}

// TestParseBlockID_Invalid asserts the canonical parser rejects malformed
// inputs that the superseded per-site parsers either silently accepted or
// handled via sentinel zero-values (T-08-15-01 mitigation).
func TestParseBlockID_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		blockID string
	}{
		{name: "missing slash", blockID: "onlyOneSegment"},
		{name: "empty string", blockID: ""},
		{name: "trailing slash (no idx)", blockID: "export/file.txt/"},
		{name: "non-integer idx", blockID: "export/file.txt/abc"},
		{name: "negative idx", blockID: "export/file.txt/-1"},
		{name: "leading slash only", blockID: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, idx, err := ParseBlockID(tt.blockID)
			if err == nil {
				t.Fatalf("expected error for %q, got payloadID=%q blockIdx=%d", tt.blockID, pid, idx)
			}
		})
	}
}
