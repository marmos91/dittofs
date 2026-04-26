package blockstore

import (
	"errors"
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

// blake3EmptyHex is the BLAKE3-256 of the empty input — used as a known
// vector for the FormatCASKey/ParseCASKey round-trip tests (BSCAS-01, D-29).
const blake3EmptyHex = "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"

// TestFormatCASKey asserts FormatCASKey returns exactly
// "cas/{hex[0:2]}/{hex[2:4]}/{hex}" for both the all-zero hash and a
// known-vector hash. See BSCAS-01.
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
// FormatCASKey and returns the original hash unchanged. See BSCAS-01.
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

// TestBlockStateConstants asserts the post-Phase-11 collapsed state machine:
// exactly three named constants Pending=0, Syncing=1, Remote=2 with matching
// String() output. Pending=0 is the safe default for legacy zero-valued rows
// (D-12). See STATE-01.
func TestBlockStateConstants(t *testing.T) {
	if BlockStatePending != 0 {
		t.Errorf("BlockStatePending = %d, want 0", BlockStatePending)
	}
	if BlockStateSyncing != 1 {
		t.Errorf("BlockStateSyncing = %d, want 1", BlockStateSyncing)
	}
	if BlockStateRemote != 2 {
		t.Errorf("BlockStateRemote = %d, want 2", BlockStateRemote)
	}

	cases := []struct {
		s    BlockState
		want string
	}{
		{BlockStatePending, "Pending"},
		{BlockStateSyncing, "Syncing"},
		{BlockStateRemote, "Remote"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("BlockState(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// TestFileBlockLastSyncAttemptAt asserts the new field exists on the
// FileBlock zero value as a zero time.Time (D-13/D-14: janitor uses this to
// requeue stale Syncing rows; never-attempted = zero value).
func TestFileBlockLastSyncAttemptAt(t *testing.T) {
	var fb FileBlock
	if !fb.LastSyncAttemptAt.IsZero() {
		t.Fatalf("FileBlock zero value LastSyncAttemptAt = %v, want zero", fb.LastSyncAttemptAt)
	}
}

// TestErrCASSentinels asserts the new exported sentinels exist, are
// distinct, self-identical via errors.Is, and have non-empty messages
// prefixed with "blockstore:" — matching ErrInvalidHash / ErrBlockNotFound
// style.
func TestErrCASSentinels(t *testing.T) {
	if !errors.Is(ErrCASContentMismatch, ErrCASContentMismatch) {
		t.Error("errors.Is(ErrCASContentMismatch, ErrCASContentMismatch) = false")
	}
	if !errors.Is(ErrCASKeyMalformed, ErrCASKeyMalformed) {
		t.Error("errors.Is(ErrCASKeyMalformed, ErrCASKeyMalformed) = false")
	}
	if errors.Is(ErrCASContentMismatch, ErrCASKeyMalformed) {
		t.Error("ErrCASContentMismatch and ErrCASKeyMalformed should be distinct")
	}
	for _, err := range []error{ErrCASContentMismatch, ErrCASKeyMalformed} {
		msg := err.Error()
		if msg == "" {
			t.Errorf("sentinel error has empty message: %v", err)
		}
		if !strings.HasPrefix(msg, "blockstore:") {
			t.Errorf("sentinel error message %q does not start with %q", msg, "blockstore:")
		}
	}
}
