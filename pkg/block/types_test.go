package block

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestContentHash_CASKey_Format is the FIX-9 explicit format guard
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
// for a known hash pattern. ships the helper ahead of the
// CAS write-path wiring.
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

// TestParseStoreKey_RoundTrip was deleted in alongside the
// block.ParseStoreKey helper it covered. The legacy
// "{payloadID}/block-{N}" key shape is gone post-CAS — content
// addressing supersedes the path-keyed form.

// TestParseBlockID_RoundTrip covers the canonical internal blockID parser
// (format: "{payloadID}/{blockIdx}"). Part of consolidation (5 -> 2).
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
// handled via sentinel zero-values (mitigation).
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
// vector for the FormatCASKey/ParseCASKey round-trip tests.
const blake3EmptyHex = "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"

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

// TestBlockStateConstants asserts the collapsed state machine
// exposes exactly three named constants Pending=0, Syncing=1,
// Remote=2 with matching String() output. Pending=0 is the safe
// default for legacy zero-valued rows.
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
// FileBlock zero value as a zero time.Time (janitor uses this to
// requeue stale Syncing rows; never-attempted = zero value).
func TestFileBlockLastSyncAttemptAt(t *testing.T) {
	var fb FileBlock
	if !fb.LastSyncAttemptAt.IsZero() {
		t.Fatalf("FileBlock zero value LastSyncAttemptAt = %v, want zero", fb.LastSyncAttemptAt)
	}
}

// TestErrCASSentinels asserts the CAS sentinels exist, are distinct,
// self-identical via errors.Is, and have non-empty messages prefixed
// with "blockstore:" (the package-qualified style used by
// ErrCASContentMismatch and ErrCASKeyMalformed).
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

// TestBlockRef_JSON exercises BlockRef zero-value invariants, JSON
// round-trip, and slice ordering preservation..
func TestBlockRef_JSON(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		var br BlockRef
		if !br.Hash.IsZero() {
			t.Errorf("zero BlockRef Hash IsZero() = false, want true")
		}
		if br.Offset != 0 {
			t.Errorf("zero BlockRef Offset = %d, want 0", br.Offset)
		}
		if br.Size != 0 {
			t.Errorf("zero BlockRef Size = %d, want 0", br.Size)
		}
	})

	t.Run("marshal known vector", func(t *testing.T) {
		hash, err := ParseContentHash(blake3EmptyHex)
		if err != nil {
			t.Fatalf("setup: ParseContentHash: %v", err)
		}
		br := BlockRef{Hash: hash, Offset: 4194304, Size: 1048576}
		got, err := json.Marshal(br)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		want := `{"hash":"blake3:af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262","offset":4194304,"size":1048576}`
		if string(got) != want {
			t.Fatalf("json.Marshal:\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("round trip preserves fields", func(t *testing.T) {
		hash, err := ParseContentHash(blake3EmptyHex)
		if err != nil {
			t.Fatalf("setup: ParseContentHash: %v", err)
		}
		want := BlockRef{Hash: hash, Offset: 4194304, Size: 1048576}
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var got BlockRef
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if !bytes.Equal(got.Hash[:], want.Hash[:]) {
			t.Errorf("Hash mismatch:\n got: %x\nwant: %x", got.Hash[:], want.Hash[:])
		}
		if got.Offset != want.Offset {
			t.Errorf("Offset = %d, want %d", got.Offset, want.Offset)
		}
		if got.Size != want.Size {
			t.Errorf("Size = %d, want %d", got.Size, want.Size)
		}
	})

	t.Run("slice round trip preserves order", func(t *testing.T) {
		mkHash := func(seed byte) ContentHash {
			var h ContentHash
			for i := range h {
				h[i] = seed
			}
			return h
		}
		want := []BlockRef{
			{Hash: mkHash(0x11), Offset: 0, Size: 4 << 20},
			{Hash: mkHash(0x22), Offset: 4 << 20, Size: 4 << 20},
			{Hash: mkHash(0x33), Offset: 8 << 20, Size: 1 << 20},
		}
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var got []BlockRef
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i].Hash[:], want[i].Hash[:]) {
				t.Errorf("[%d] Hash mismatch:\n got: %x\nwant: %x", i, got[i].Hash[:], want[i].Hash[:])
			}
			if got[i].Offset != want[i].Offset {
				t.Errorf("[%d] Offset = %d, want %d", i, got[i].Offset, want[i].Offset)
			}
			if got[i].Size != want[i].Size {
				t.Errorf("[%d] Size = %d, want %d", i, got[i].Size, want[i].Size)
			}
		}
	})

	t.Run("rejects bad hash length", func(t *testing.T) {
		// T-12-01 mitigation: a tampered short-hex hash must not deserialize.
		bad := `{"hash":"blake3:deadbeef","offset":0,"size":0}`
		var br BlockRef
		if err := json.Unmarshal([]byte(bad), &br); err == nil {
			t.Fatalf("expected error for short hash, got nil (br=%+v)", br)
		}
	})
}

// TestContentHash_JSONBackwardCompat asserts UnmarshalJSON accepts both
// the new canonical "blake3:{hex}" form and the legacy default base64
// form that encoding/json produced for [32]byte before added a
// MarshalJSON. Critical for reading FileBlock rows persisted by
// Badger backends.
func TestContentHash_JSONBackwardCompat(t *testing.T) {
	hash, err := ParseContentHash(blake3EmptyHex)
	if err != nil {
		t.Fatalf("setup: ParseContentHash: %v", err)
	}

	// Default base64 encoding of the 32 raw hash bytes (legacy wire form).
	legacyB64 := `"rxNJufX5oaagQE3qNtzJSZvLJcmtwRK3zJqTyuQfMmI="`
	var got ContentHash
	if err := got.UnmarshalJSON([]byte(legacyB64)); err != nil {
		t.Fatalf("UnmarshalJSON legacy base64: %v", err)
	}
	if !bytes.Equal(got[:], hash[:]) {
		t.Fatalf("legacy base64 decode mismatch:\n got: %x\nwant: %x", got[:], hash[:])
	}

	// New canonical form round-trips.
	gotCanonical := ContentHash{}
	if err := gotCanonical.UnmarshalJSON([]byte(`"blake3:` + blake3EmptyHex + `"`)); err != nil {
		t.Fatalf("UnmarshalJSON canonical: %v", err)
	}
	if !bytes.Equal(gotCanonical[:], hash[:]) {
		t.Fatalf("canonical decode mismatch:\n got: %x\nwant: %x", gotCanonical[:], hash[:])
	}

	// Bare hex form accepted too.
	gotBareHex := ContentHash{}
	if err := gotBareHex.UnmarshalJSON([]byte(`"` + blake3EmptyHex + `"`)); err != nil {
		t.Fatalf("UnmarshalJSON bare hex: %v", err)
	}
	if !bytes.Equal(gotBareHex[:], hash[:]) {
		t.Fatalf("bare hex decode mismatch:\n got: %x\nwant: %x", gotBareHex[:], hash[:])
	}
}

// TestContentHash_JSONBackwardCompat_V014Array asserts UnmarshalJSON accepts
// the JSON number-array form that v0.14.x and earlier serialized for
// ContentHash (encoding/json's default for [32]byte when no custom
// MarshalJSON existed). Critical for reading badger metadata written by
// v0.14.2 servers during upgrade migration.
func TestContentHash_JSONBackwardCompat_V014Array(t *testing.T) {
	// v0.14.x zero-value ContentHash serialized as [0,0,...,0]
	zeroArr := "[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]"
	var got ContentHash
	if err := got.UnmarshalJSON([]byte(zeroArr)); err != nil {
		t.Fatalf("UnmarshalJSON v0.14 zero array: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("expected zero hash, got %x", got[:])
	}

	// Non-zero array (simulates a v0.14.x row with actual content hash)
	nonZeroArr := "[175,19,73,185,245,249,161,166,160,64,77,234,54,220,201,73,155,203,37,201,173,193,18,183,204,154,147,202,228,31,50,98]"
	var got2 ContentHash
	if err := got2.UnmarshalJSON([]byte(nonZeroArr)); err != nil {
		t.Fatalf("UnmarshalJSON v0.14 non-zero array: %v", err)
	}
	want, err := ParseContentHash(blake3EmptyHex)
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	if !bytes.Equal(got2[:], want[:]) {
		t.Fatalf("v0.14 non-zero array decode mismatch:\n got: %x\nwant: %x", got2[:], want[:])
	}
}

// TestErrBlockRefMissing asserts the new sentinel exists, is self-identical
// via errors.Is, and has the expected message style ("blockstore:" prefix
// + mentions "block ref")..
func TestErrBlockRefMissing(t *testing.T) {
	if !errors.Is(ErrBlockRefMissing, ErrBlockRefMissing) {
		t.Error("errors.Is(ErrBlockRefMissing, ErrBlockRefMissing) = false")
	}
	if errors.Is(ErrBlockRefMissing, ErrCASKeyMalformed) {
		t.Error("ErrBlockRefMissing should be distinct from ErrCASKeyMalformed")
	}
	msg := ErrBlockRefMissing.Error()
	if !strings.HasPrefix(msg, "blockstore:") {
		t.Errorf("ErrBlockRefMissing.Error() = %q, want prefix %q", msg, "blockstore:")
	}
	if !strings.Contains(strings.ToLower(msg), "block ref") {
		t.Errorf("ErrBlockRefMissing.Error() = %q, want it to mention %q", msg, "block ref")
	}
}

func TestPruneBlockRefsToSize(t *testing.T) {
	ref := func(off uint64, sz uint32) BlockRef { return BlockRef{Offset: off, Size: sz} }
	const mib = uint64(1 << 20)

	tests := []struct {
		name    string
		refs    []BlockRef
		size    uint64
		want    []uint64 // expected surviving offsets, sorted
		wantNil bool
	}{
		{
			name: "truncate 4MiB to 1MiB keeps only first block",
			refs: []BlockRef{ref(0, uint32(mib)), ref(mib, uint32(mib)), ref(2*mib, uint32(mib)), ref(3*mib, uint32(mib))},
			size: mib,
			want: []uint64{0},
		},
		{
			name: "block straddling new EOF is kept",
			refs: []BlockRef{ref(0, uint32(mib)), ref(mib, uint32(mib))},
			size: mib + 100,
			want: []uint64{0, mib},
		},
		{
			name: "ref starting exactly at new size is dropped",
			refs: []BlockRef{ref(0, uint32(mib)), ref(mib, uint32(mib))},
			size: mib,
			want: []uint64{0},
		},
		{
			name:    "truncate to zero drops all and returns nil",
			refs:    []BlockRef{ref(0, uint32(mib))},
			size:    0,
			wantNil: true,
		},
		{
			name: "result is sorted by offset",
			refs: []BlockRef{ref(2*mib, uint32(mib)), ref(0, uint32(mib)), ref(mib, uint32(mib))},
			size: 3 * mib,
			want: []uint64{0, mib, 2 * mib},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := make([]BlockRef, len(tc.refs))
			copy(in, tc.refs)

			got := PruneBlockRefsToSize(tc.refs, tc.size)

			if tc.wantNil {
				if got != nil {
					t.Errorf("PruneBlockRefsToSize() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.want), got)
			}
			for i, off := range tc.want {
				if got[i].Offset != off {
					t.Errorf("got[%d].Offset = %d, want %d", i, got[i].Offset, off)
				}
				if got[i].Offset >= tc.size {
					t.Errorf("got[%d].Offset %d is at/past size %d", i, got[i].Offset, tc.size)
				}
			}
			// Input must not be mutated.
			for i := range in {
				if tc.refs[i] != in[i] {
					t.Errorf("input mutated at %d", i)
				}
			}
		})
	}
}
