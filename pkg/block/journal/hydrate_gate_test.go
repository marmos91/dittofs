package journal

import (
	"bytes"
	"context"
	"testing"
)

// TestHydrateGate covers the WriteVersion bound on a hydrate's write-back: a
// mark taken before the range was written keeps the fetched bytes out, a mark
// taken after lets them in, and an unmarked hydrate is ungated.
func TestHydrateGate(t *testing.T) {
	ctx := context.Background()
	stale := bytes.Repeat([]byte{0xAB}, 4096)
	fresh := bytes.Repeat([]byte{0x00}, 4096)

	t.Run("mark predating the write drops it", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, stale); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		mark := s.WriteVersion() // a fetch resolves here
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Hydrate(ctx, "f", 0, stale, mark); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		got := make([]byte, len(fresh))
		if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if !bytes.Equal(got, fresh) {
			t.Fatalf("stale hydrate won over a newer write")
		}
	})

	t.Run("mark after the write lets it through", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		mark := s.WriteVersion()
		if err := s.Hydrate(ctx, "f", 0, stale, mark); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		got := make([]byte, len(stale))
		if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if !bytes.Equal(got, stale) {
			t.Fatalf("hydrate marked after the write was dropped")
		}
	})

	t.Run("zero mark is ungated", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Hydrate(ctx, "f", 0, stale, 0); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		got := make([]byte, len(stale))
		if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if !bytes.Equal(got, stale) {
			t.Fatalf("ungated hydrate was dropped")
		}
	})
}
