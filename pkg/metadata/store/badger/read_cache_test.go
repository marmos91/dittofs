package badger

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func putFileForTest(t testing.TB, s *BadgerMetadataStore, share, path string, mode uint32, nBlocks int) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	handle, err := s.GenerateHandle(ctx, share, path)
	if err != nil {
		t.Fatal(err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	f := bigFile(nBlocks)
	f.ID = id
	f.ShareName = share
	f.Path = path
	f.Mode = mode
	if err := s.PutFile(ctx, f); err != nil {
		t.Fatal(err)
	}
	return handle
}

// TestReadCache_NoStaleReadAfterMutation is the #1169 guard: a read after a write
// must never return the pre-write value, even though the first read cached it.
func TestReadCache_NoStaleReadAfterMutation(t *testing.T) {
	s, err := NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	handle := putFileForTest(t, s, "/s", "/f", 0o644, 4)

	got, err := s.GetFileForRead(ctx, handle) // populates the cache
	if err != nil || got.Mode != 0o644 {
		t.Fatalf("first read: mode=%o err=%v", got.Mode, err)
	}

	// Mutate via PutFile — must invalidate the cache.
	_, id, _ := metadata.DecodeFileHandle(handle)
	f := bigFile(4)
	f.ID, f.ShareName, f.Path, f.Mode = id, "/s", "/f", 0o600
	if err := s.PutFile(ctx, f); err != nil {
		t.Fatal(err)
	}

	got, err = s.GetFileForRead(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != 0o600 {
		t.Fatalf("STALE READ: got mode %o after write set 0o600", got.Mode)
	}

	// A returned File must be a copy — mutating it (scalar OR reference-bearing
	// fields) must not corrupt the shared cache entry. Before the cache, badger
	// JSON-decoded a fresh File per read, so callers never aliased stored state.
	got.Mode = 0o000
	if len(got.Blocks) > 0 {
		got.Blocks[0].Offset = 0xdeadbeef // in-place mutation of the block slice
	}
	again, _ := s.GetFileForRead(ctx, handle)
	if again.Mode != 0o600 {
		t.Fatalf("cache corrupted by caller scalar mutation: got %o", again.Mode)
	}
	if len(again.Blocks) > 0 && again.Blocks[0].Offset == 0xdeadbeef {
		t.Fatal("cache Blocks aliased: caller in-place edit leaked into the cached File")
	}
}

// BenchmarkGetFileForRead_Cached measures the warm read path on a 1024-block
// file — a cache hit skips the ~800 µs badger-read + JSON decode entirely.
func BenchmarkGetFileForRead_Cached(b *testing.B) {
	s, err := NewBadgerMetadataStoreWithDefaults(context.Background(), b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	handle := putFileForTest(b, s, "/s", "/big", 0o644, 1024)
	if _, err := s.GetFileForRead(ctx, handle); err != nil { // warm the cache
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetFileForRead(ctx, handle); err != nil {
			b.Fatal(err)
		}
	}
}
