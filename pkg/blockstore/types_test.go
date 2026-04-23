package blockstore

import (
	"testing"
)

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
