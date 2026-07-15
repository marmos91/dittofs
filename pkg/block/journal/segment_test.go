package journal

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
)

// TestSealRotateAcrossSegments forces the active segment past its cap so it
// seals and a new one opens, then confirms a file whose bytes straddle the
// sealed and active segments reads back byte-identical.
func TestSealRotateAcrossSegments(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()

	chunk := 64 << 10
	data := func(i int) []byte { return bytes.Repeat([]byte{byte('A' + i%26)}, chunk) }

	const spans = 40 // ~2.5 MiB into a 1 MiB segment: several rotations
	whole := make([]byte, 0, spans*chunk)
	for i := 0; i < spans; i++ {
		d := data(i)
		if err := s.WriteAt(ctx, "big", int64(i*chunk), d); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
		whole = append(whole, d...)
	}

	if segs := s.Stats().Segments; segs < 2 {
		t.Fatalf("expected multiple segments after rotation, got %d", segs)
	}

	got := make([]byte, len(whole))
	if _, _, err := s.ReadAt(ctx, "big", 0, got); err != nil {
		t.Fatalf("ReadAt whole: %v", err)
	}
	if !bytes.Equal(got, whole) {
		t.Fatalf("readback across sealed+active segments mismatch")
	}

	// A read that lands squarely inside a sealed (rotated-away) segment must
	// still resolve through the pooled read-only fd.
	sub := make([]byte, 100)
	if _, _, err := s.ReadAt(ctx, "big", 500, sub); err != nil {
		t.Fatalf("ReadAt sealed sub-range: %v", err)
	}
	if !bytes.Equal(sub, whole[500:600]) {
		t.Fatalf("sealed sub-range mismatch")
	}
}

// TestManyFileIDsByteIdentical round-trips arbitrary FileID lengths at arbitrary
// offsets and lengths, asserting exact byte equality end to end.
func TestManyFileIDsByteIdentical(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))

	type span struct {
		id      FileID
		offset  int64
		payload []byte
	}
	var spans []span
	for i := 0; i < 200; i++ {
		idLen := 1 + rng.Intn(300) // arbitrary FileID length, incl. long IDs
		idb := make([]byte, idLen)
		for j := range idb {
			idb[j] = byte('a' + rng.Intn(26))
		}
		id := FileID(fmt.Sprintf("%s-%d", idb, i)) // unique per span
		payload := make([]byte, 1+rng.Intn(9000))
		rng.Read(payload)
		offset := rng.Int63n(1 << 30)
		if err := s.WriteAt(ctx, id, offset, payload); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		spans = append(spans, span{id, offset, payload})
	}

	for _, sp := range spans {
		got := make([]byte, len(sp.payload))
		if _, _, err := s.ReadAt(ctx, sp.id, sp.offset, got); err != nil {
			t.Fatalf("ReadAt %q@%d: %v", sp.id, sp.offset, err)
		}
		if !bytes.Equal(got, sp.payload) {
			t.Fatalf("byte mismatch for %q@%d (len %d)", sp.id, sp.offset, len(sp.payload))
		}
	}
}

// TestConcurrentAppendReadRace runs writers and readers over shared shards to
// exercise the brief-lock-then-unlocked-pread path against concurrent appends.
// Its value is under `go test -race`.
func TestConcurrentAppendReadRace(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize})
	ctx := context.Background()

	const writers = 8
	const perWriter = 64
	chunk := 4 << 10

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := FileID(fmt.Sprintf("file-%d", w))
			data := bytes.Repeat([]byte{byte('a' + w)}, chunk)
			for i := 0; i < perWriter; i++ {
				if err := s.WriteAt(ctx, id, int64(i*chunk), data); err != nil {
					t.Errorf("WriteAt: %v", err)
					return
				}
				if i%8 == 0 {
					if err := s.Commit(ctx, id); err != nil {
						t.Errorf("Commit: %v", err)
						return
					}
				}
			}
		}(w)
	}
	// Concurrent readers over the same files being appended. Ranges not yet
	// written come back as zero-filled holes; the point is no race, no error.
	for r := 0; r < writers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			id := FileID(fmt.Sprintf("file-%d", r))
			dst := make([]byte, chunk)
			for i := 0; i < perWriter; i++ {
				if _, _, err := s.ReadAt(ctx, id, int64((i%perWriter)*chunk), dst); err != nil {
					t.Errorf("ReadAt: %v", err)
					return
				}
			}
		}(r)
	}
	wg.Wait()

	// Final readback: every writer's bytes are intact.
	for w := 0; w < writers; w++ {
		id := FileID(fmt.Sprintf("file-%d", w))
		want := bytes.Repeat([]byte{byte('a' + w)}, chunk)
		got := make([]byte, chunk)
		if _, _, err := s.ReadAt(ctx, id, int64((perWriter-1)*chunk), got); err != nil {
			t.Fatalf("final ReadAt: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("final byte mismatch for %q", id)
		}
	}
}
