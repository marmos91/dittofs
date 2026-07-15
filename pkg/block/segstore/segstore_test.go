package segstore

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func testStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), cfg, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAppendReadRoundTrip(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	payload := bytes.Repeat([]byte("dittofs-segstore-"), 4096) // ~68 KiB
	if err := s.WriteAt(ctx, "file-a", 0, payload); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Full read back.
	got := make([]byte, len(payload))
	n, cold, err := s.ReadAt(ctx, "file-a", 0, got)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if cold {
		t.Fatalf("unexpected cold read")
	}
	if n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: n=%d", n)
	}

	// Sub-range read at an offset.
	sub := make([]byte, 100)
	if _, _, err := s.ReadAt(ctx, "file-a", 500, sub); err != nil {
		t.Fatalf("ReadAt sub: %v", err)
	}
	if !bytes.Equal(sub, payload[500:600]) {
		t.Fatalf("sub-range mismatch")
	}
}

func TestReadHoleZeroFilled(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "sparse", 4096, []byte("tail")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	// Read spanning the leading hole and the written tail.
	dst := make([]byte, 4100)
	for i := range dst {
		dst[i] = 0xFF // ensure holes are actively zeroed, not left as-is
	}
	if _, _, err := s.ReadAt(ctx, "sparse", 0, dst); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	for i := 0; i < 4096; i++ {
		if dst[i] != 0 {
			t.Fatalf("hole byte %d not zeroed: %#x", i, dst[i])
		}
	}
	if !bytes.Equal(dst[4096:], []byte("tail")) {
		t.Fatalf("tail mismatch: %q", dst[4096:])
	}
}

func TestNewestWinsOverwrite(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, bytes.Repeat([]byte("A"), 10)); err != nil {
		t.Fatal(err)
	}
	// Overlapping newer write covers the middle.
	if err := s.WriteAt(ctx, "f", 3, []byte("BBBB")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 10)
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatal(err)
	}
	if want := "AAABBBBAAA"; string(got) != want {
		t.Fatalf("newest-wins mismatch: got %q want %q", got, want)
	}
}

func TestDataExtents(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	_ = s.WriteAt(ctx, "f", 0, bytes.Repeat([]byte("x"), 100))
	_ = s.WriteAt(ctx, "f", 100, bytes.Repeat([]byte("y"), 50)) // adjacent -> merges
	_ = s.WriteAt(ctx, "f", 1000, bytes.Repeat([]byte("z"), 10))

	ext, err := s.DataExtents(ctx, "f", 2000)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]uint64{{0, 150}, {1000, 1010}}
	if len(ext) != len(want) {
		t.Fatalf("extents=%v want %v", ext, want)
	}
	for i := range want {
		if ext[i] != want[i] {
			t.Fatalf("extent %d = %v want %v", i, ext[i], want[i])
		}
	}
}

func TestShardForDeterministicAndMasked(t *testing.T) {
	s := testStore(t, Config{ShardCount: 8})
	seen := map[FileID]*shard{}
	for _, id := range []FileID{"a", "b", "c", "payload-0001", "payload-0002", ""} {
		sh := s.shardFor(id)
		if sh == nil {
			t.Fatalf("nil shard for %q", id)
		}
		if prev, ok := seen[id]; ok && prev != sh {
			t.Fatalf("shardFor(%q) not deterministic", id)
		}
		seen[id] = sh
	}
	// Hash must land within the shard slice for many keys.
	for i := 0; i < 1000; i++ {
		id := FileID("k" + time.Duration(i).String())
		if s.shardFor(id) == nil {
			t.Fatalf("out-of-range shard for %q", id)
		}
	}
}

func TestReopenPopulatedDirRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = s.Close()
	if _, err := Open(dir, Config{}, newFakeRemote(), SystemClock()); err == nil {
		t.Fatalf("expected reopen to be refused")
	}
}

func TestRecordHeaderCodecCRCExcludesFlags(t *testing.T) {
	fileID := []byte("some-file-id")
	h := recordHeader{
		FileIDLen:  uint16(len(fileID)),
		FileOffset: 123456,
		PayloadLen: 4096,
		Version:    99,
		Flags:      0, // encode with synced=false
	}
	buf := encodeHeader(h, fileID)

	got, err := decodeHeader(buf)
	if err != nil {
		t.Fatalf("decodeHeader: %v", err)
	}
	if got != h {
		t.Fatalf("decode mismatch: %+v want %+v", got, h)
	}

	// Flip the synced flag in place; the header CRC must still validate
	// because it deliberately excludes the Flags byte.
	buf[24] |= flagSynced
	got2, err := decodeHeader(buf)
	if err != nil {
		t.Fatalf("decodeHeader after flag flip: %v", err)
	}
	if got2.Flags&flagSynced == 0 {
		t.Fatalf("flag flip not observed")
	}

	// Corrupting a CRC-covered field must fail validation.
	buf[4] ^= 0xFF
	if _, err := decodeHeader(buf); err == nil {
		t.Fatalf("expected CRC failure on corrupted header")
	}
}
