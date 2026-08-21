package journal

import (
	"bytes"
	"context"
	"testing"
)

// TestHydrateFillsWithoutOverwriting covers what a hydrate is allowed to write
// back: live local bytes are never replaced whatever the mark, a cold range is
// filled only when it predates the mark, and a range the file does not hold is
// always filled.
func TestHydrateFillsWithoutOverwriting(t *testing.T) {
	ctx := context.Background()
	stale := bytes.Repeat([]byte{0xAB}, 4096)
	fresh := bytes.Repeat([]byte{0x00}, 4096)

	read := func(t *testing.T, s *Store, n int) []byte {
		t.Helper()
		got := make([]byte, n)
		if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		return got
	}

	t.Run("live bytes survive an unmarked hydrate", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Hydrate(ctx, "f", 0, stale, 0); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !bytes.Equal(read(t, s, len(fresh)), fresh) {
			t.Fatal("hydrate overwrote live local bytes")
		}
	})

	t.Run("live bytes survive a hydrate marked after them", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Hydrate(ctx, "f", 0, stale, s.WriteVersion()); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !bytes.Equal(read(t, s, len(fresh)), fresh) {
			t.Fatal("hydrate overwrote live local bytes")
		}
	})

	t.Run("a hole is filled", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.Hydrate(ctx, "f", 0, stale, s.WriteVersion()); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !bytes.Equal(read(t, s, len(stale)), stale) {
			t.Fatal("hydrate of a hole was dropped")
		}
	})

	t.Run("a cold range is filled only when it predates the mark", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		// An unrelated record first, so the mark taken before "f" is written is
		// still non-zero — a zero mark means "no bound", not "the beginning".
		if err := s.WriteAt(ctx, "g", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		markBefore := s.WriteVersion()
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		markAfter := s.WriteVersion()
		sh := s.shardFor("f")
		sh.mu.Lock()
		for i := range sh.index["f"].ivs {
			sh.index["f"].ivs[i].cold = true
		}
		sh.mu.Unlock()

		if err := s.Hydrate(ctx, "f", 0, stale, markBefore); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if _, st, err := s.ReadAt(ctx, "f", 0, make([]byte, len(stale))); err != nil {
			t.Fatalf("ReadAt: %v", err)
		} else if !st.Cold {
			t.Fatal("hydrate marked before the cold range was recorded was not dropped")
		}

		if err := s.Hydrate(ctx, "f", 0, stale, markAfter); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !bytes.Equal(read(t, s, len(stale)), stale) {
			t.Fatal("hydrate of a cold range predating the mark was dropped")
		}
	})

	t.Run("only the cold part of a straddling range is filled", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil { // [0, 4096) stays live
			t.Fatalf("WriteAt: %v", err)
		}
		both := append(append([]byte{}, stale...), stale...)
		if err := s.Hydrate(ctx, "f", 0, both, s.WriteVersion()); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		got := read(t, s, len(both))
		if !bytes.Equal(got[:len(fresh)], fresh) {
			t.Fatal("hydrate overwrote the live head")
		}
		if !bytes.Equal(got[len(fresh):], stale) {
			t.Fatal("hydrate did not fill the hole past the live head")
		}
	})
}
