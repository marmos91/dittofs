package metadata

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestFileAttr_BlocksRoundTrip exercises the reintroduction of
// FileAttr.Blocks []block.BlockRef. Asserts:
//
//  1. omitempty: zero-value FileAttr does not emit a "blocks" key.
//  2. Round trip preserves order and all fields of every BlockRef.
//  3. Legacy JSON without a "blocks" key deserializes cleanly to nil
//
// (forward compat for files written before dual-read
//
//	shim trigger).
func TestFileAttr_BlocksRoundTrip(t *testing.T) {
	t.Run("omitempty when nil", func(t *testing.T) {
		var fa FileAttr
		raw, err := json.Marshal(fa)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var asMap map[string]any
		if err := json.Unmarshal(raw, &asMap); err != nil {
			t.Fatalf("json.Unmarshal map: %v", err)
		}
		if _, ok := asMap["blocks"]; ok {
			t.Errorf("zero-value FileAttr serialized with \"blocks\" key (raw=%s)", raw)
		}
	})

	t.Run("round trip preserves order", func(t *testing.T) {
		mkHash := func(seed byte) block.ContentHash {
			var h block.ContentHash
			for i := range h {
				h[i] = seed
			}
			return h
		}
		want := []block.BlockRef{
			{Hash: mkHash(0xAA), Offset: 0, Size: 4 << 20},
			{Hash: mkHash(0xBB), Offset: 4 << 20, Size: 4 << 20},
			{Hash: mkHash(0xCC), Offset: 8 << 20, Size: 1 << 20},
		}
		fa := FileAttr{
			Type:   FileTypeRegular,
			Mode:   0o644,
			Size:   9 << 20,
			Blocks: want,
		}
		raw, err := json.Marshal(fa)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var got FileAttr
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if len(got.Blocks) != len(want) {
			t.Fatalf("len(Blocks) = %d, want %d", len(got.Blocks), len(want))
		}
		for i := range want {
			if !bytes.Equal(got.Blocks[i].Hash[:], want[i].Hash[:]) {
				t.Errorf("Blocks[%d] Hash mismatch:\n got: %x\nwant: %x", i, got.Blocks[i].Hash[:], want[i].Hash[:])
			}
			if got.Blocks[i].Offset != want[i].Offset {
				t.Errorf("Blocks[%d] Offset = %d, want %d", i, got.Blocks[i].Offset, want[i].Offset)
			}
			if got.Blocks[i].Size != want[i].Size {
				t.Errorf("Blocks[%d] Size = %d, want %d", i, got.Blocks[i].Size, want[i].Size)
			}
		}
	})

	t.Run("legacy JSON without blocks key deserializes nil", func(t *testing.T) {
		// Realistic FileAttr blob — no "blocks" key. T-12-03
		// mitigation: must NOT error and must yield nil Blocks (triggers
		// the dual-read shim per).
		legacy := `{"type":0,"mode":420,"uid":1000,"gid":1000,"nlink":1,"size":42,"atime":"2026-01-01T00:00:00Z","mtime":"2026-01-01T00:00:00Z","ctime":"2026-01-01T00:00:00Z","creation_time":"2026-01-01T00:00:00Z","content_id":"share/file.bin"}`
		var fa FileAttr
		if err := json.Unmarshal([]byte(legacy), &fa); err != nil {
			t.Fatalf("json.Unmarshal legacy: %v", err)
		}
		if fa.Blocks != nil {
			t.Errorf("legacy FileAttr.Blocks = %v, want nil", fa.Blocks)
		}
		if fa.Size != 42 {
			t.Errorf("legacy Size = %d, want 42 (proves rest of struct decoded)", fa.Size)
		}
	})

	t.Run("blocks field omits when explicitly empty slice", func(t *testing.T) {
		// omitempty omits both nil AND zero-length slices.
		fa := FileAttr{Blocks: []block.BlockRef{}}
		raw, err := json.Marshal(fa)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if strings.Contains(string(raw), `"blocks"`) {
			t.Errorf("explicit empty Blocks emitted \"blocks\" key (raw=%s)", raw)
		}
	})
}
