package journal

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

// TestHydrateAfterDeleteIsDropped is the delete twin of
// TestHydrateAfterTruncateIsDropped. A delete removes the file's index entry
// outright, so hydratable's nil-receiver path offers the whole requested range
// and a hydrate still in flight from before the delete would re-create the file
// out of pre-delete remote bytes.
func TestHydrateAfterDeleteIsDropped(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	data := bytes.Repeat([]byte{0xAB}, 4096)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	mark := s.WriteVersion() // a fetch resolves the pre-delete manifest here
	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Hydrate(ctx, "f", 0, data, mark); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if sz, ok := s.FileSize(ctx, "f"); ok {
		t.Fatalf("hydrate resurrected a deleted file: FileSize = %d", sz)
	}
	got := make([]byte, len(data))
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, make([]byte, len(data))) {
		t.Fatal("hydrate resurrected bytes a delete removed")
	}
}

// TestHydrateRacingDeleteNeverResurrects runs the two against each other rather
// than in a fixed order. Whichever wins the shard lock, a hydrate whose bound
// was sampled before the delete must leave nothing behind: it either loses the
// fence check or lands below the tombstone's Version and is buried by it.
func TestHydrateRacingDeleteNeverResurrects(t *testing.T) {
	ctx := context.Background()
	data := bytes.Repeat([]byte{0xAB}, 4096)
	for i := 0; i < 64; i++ {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, data); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		mark := s.WriteVersion()

		var wg sync.WaitGroup
		var delErr, hydErr error
		wg.Add(2)
		go func() { defer wg.Done(); delErr = s.Delete(ctx, "f") }()
		go func() { defer wg.Done(); hydErr = s.Hydrate(ctx, "f", 0, data, mark) }()
		wg.Wait()
		if delErr != nil {
			t.Fatalf("Delete: %v", delErr)
		}
		if hydErr != nil {
			t.Fatalf("Hydrate: %v", hydErr)
		}
		if sz, ok := s.FileSize(ctx, "f"); ok {
			t.Fatalf("iteration %d: hydrate resurrected a deleted file: FileSize = %d", i, sz)
		}
		_ = s.Close()
	}
}

// TestHydrateAfterDeleteAndRecreate pins the other side of the fence: it bounds
// hydrates that predate the delete, not the file name forever. A file written
// again after the delete accepts a hydrate whose bound was sampled after it.
func TestHydrateAfterDeleteAndRecreate(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	head := bytes.Repeat([]byte{0x11}, 4096)
	tail := bytes.Repeat([]byte{0x22}, 4096)

	if err := s.WriteAt(ctx, "f", 0, head); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.WriteAt(ctx, "f", 0, head); err != nil { // re-created after the unlink
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Hydrate(ctx, "f", int64(len(head)), tail, s.WriteVersion()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	got := make([]byte, len(head)+len(tail))
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[len(head):], tail) {
		t.Fatal("the delete fence outlived the file and blocked a hydrate of its replacement")
	}
}
