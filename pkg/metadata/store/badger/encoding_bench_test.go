package badger

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// bigFile builds a File with nBlocks ChunkRefs — the shape GetFileForRead
// decodes on every read of a rolled-up file (a 1 GiB file at the ~1 MiB FastCDC
// average is ~1024 chunks). The inline Blocks list dominates the JSON decode.
func bigFile(nBlocks int) *metadata.File {
	blocks := make([]block.ChunkRef, nBlocks)
	for i := range blocks {
		var h block.ContentHash
		for j := range h {
			h[j] = byte(i*7 + j)
		}
		blocks[i] = block.ChunkRef{Hash: h, Offset: uint64(i) << 20, Size: 1 << 20}
	}
	return &metadata.File{
		ID:        uuid.New(),
		ShareName: "/bench",
		Path:      "/data.bin",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000, Nlink: 1,
			Size: uint64(nBlocks) << 20, Atime: time.Unix(1, 0), Mtime: time.Unix(2, 0),
			Ctime: time.Unix(3, 0), CreationTime: time.Unix(4, 0),
			PayloadID: "bench/data", Blocks: blocks,
		},
	}
}

// BenchmarkDecodeFile measures decodeFile on a 1024-block File. With the
// generated easyjson UnmarshalJSON present, decodeFile's json.Unmarshal
// auto-dispatches to it; remove file_types_easyjson.go to get the reflection
// baseline. Run: go test -run=xxx -bench=BenchmarkDecodeFile ./pkg/metadata/store/badger/
func BenchmarkDecodeFile(b *testing.B) {
	enc, err := encodeFile(bigFile(1024))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(enc)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := decodeFile(enc); err != nil {
			b.Fatal(err)
		}
	}
}

// TestEncodeDecodeFile_RoundTrip guards format correctness: a File survives
// encode -> decode byte-for-byte in its fields (decodeFile zeroes Path per
// #1166, so we compare with Path cleared).
func TestEncodeDecodeFile_RoundTrip(t *testing.T) {
	orig := bigFile(37)
	orig.ACL = nil
	enc, err := encodeFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeFile(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != orig.ID || got.ShareName != orig.ShareName || got.Size != orig.Size ||
		got.Mode != orig.Mode || got.PayloadID != orig.PayloadID || len(got.Blocks) != len(orig.Blocks) {
		t.Fatalf("round-trip mismatch: got %+v", got.FileAttr)
	}
	for i := range orig.Blocks {
		if got.Blocks[i] != orig.Blocks[i] {
			t.Fatalf("block %d mismatch: got %+v want %+v", i, got.Blocks[i], orig.Blocks[i])
		}
	}
	if !got.Mtime.Equal(orig.Mtime) || !got.Ctime.Equal(orig.Ctime) {
		t.Fatalf("time mismatch: got mtime=%v ctime=%v", got.Mtime, got.Ctime)
	}
}
