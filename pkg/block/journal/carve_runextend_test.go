package journal

import (
	"context"
	"sync"
	"testing"
)

// rowEnderSink is a fakeSink that also answers the run-extension query, standing
// in for the production sink's manifest lookup.
type rowEnderSink struct {
	*fakeSink
	mu     sync.Mutex
	rowEnd int64
	asked  []int64
}

func (s *rowEnderSink) ManifestRowEndAfter(_ context.Context, _ FileID, off int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, off)
	if s.rowEnd > off {
		return s.rowEnd, nil
	}
	return off, nil
}

// carvedRange returns the range the sink's committed chunks span, so a test can
// see how far a run reached.
func carvedRange(s *fakeSink) (lo, hi int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first := true
	for off, data := range s.chunks {
		if first || off < lo {
			lo = off
		}
		if end := off + int64(len(data)); first || end > hi {
			hi = end
		}
		first = false
	}
	return lo, hi
}

// carvedBytesIn counts how many bytes of [from, to) the sink's committed chunks
// cover. Chunk boundaries are content-defined, so a run's reach has to be read
// off the bytes it covered rather than off any particular chunk offset.
func carvedBytesIn(s *fakeSink, from, to int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for off, data := range s.chunks {
		lo, hi := max(off, from), min(off+int64(len(data)), to)
		if hi > lo {
			n += hi - lo
		}
	}
	return n
}

// carveWithRowEnder wires a sink that reports rowEnd as the end of the manifest
// coverage straddling any earlier offset.
func carveWithRowEnder(t *testing.T) (*Store, *rowEnderSink) {
	t.Helper()
	s, dd, sink, _ := carveStore(t, Config{CarveBlockSize: 1 << 20})
	re := &rowEnderSink{fakeSink: sink}
	s.SetCarveTargets(dd, re)
	return s, re
}

// splitIntervals lays down interval boundaries at 8Ki, 16Ki, 24Ki and 32Ki. A
// write that partially overlaps a warm interval re-dirties its survivors, so the
// only way to leave a warm neighbour standing is to overwrite a whole interval —
// which first has to exist.
func splitIntervals(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	if err := s.WriteAt(ctx, "f", 0, randBytes(64<<10, 7)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	for _, w := range [][2]int64{{8 << 10, 8 << 10}, {24 << 10, 8 << 10}} {
		if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
			t.Fatalf("Carve: %v", err)
		}
		if err := s.WriteAt(ctx, "f", w[0], randBytes(int(w[1]), w[0])); err != nil {
			t.Fatalf("WriteAt %d: %v", w[0], err)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
}

// TestCarveRunExtendsToStraddledRowEnd pins the extension: a run whose end lands
// inside a manifest row carves on to that row's end, so the run-end reap does not
// delete a row whose tail nothing else covers.
func TestCarveRunExtendsToStraddledRowEnd(t *testing.T) {
	ctx := context.Background()
	s, sink := carveWithRowEnder(t)
	splitIntervals(t, ctx, s)

	// An exact-range overwrite of one interval leaves its neighbours warm, so the
	// dirty run stops at 16Ki — inside a row reaching to 20Ki.
	if err := s.WriteAt(ctx, "f", 8<<10, randBytes(8<<10, 11)); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	sink.chunks = map[int64][]byte{}
	sink.rowEnd = 20 << 10

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	lo, hi := carvedRange(sink.fakeSink)
	if lo != 8<<10 || hi != 20<<10 {
		t.Fatalf("carve covered [%d, %d), want [%d, %d)", lo, hi, 8<<10, 20<<10)
	}
}

// TestCarveRunDoesNotExtendIntoDirtyRange pins the refusal: the bytes past the
// run end belong to a later dirty run, so extending would carve them twice. The
// run is left as snapshotted rather than half extended.
func TestCarveRunDoesNotExtendIntoDirtyRange(t *testing.T) {
	ctx := context.Background()
	s, sink := carveWithRowEnder(t)
	splitIntervals(t, ctx, s)

	// Two dirty runs with a warm interval between them.
	if err := s.WriteAt(ctx, "f", 8<<10, randBytes(8<<10, 11)); err != nil {
		t.Fatalf("overwrite head: %v", err)
	}
	if err := s.WriteAt(ctx, "f", 24<<10, randBytes(8<<10, 12)); err != nil {
		t.Fatalf("overwrite tail: %v", err)
	}
	sink.chunks = map[int64][]byte{}
	sink.rowEnd = 28 << 10 // past the first run, into the second one

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if n := carvedBytesIn(sink.fakeSink, 16<<10, 24<<10); n != 0 {
		t.Fatalf("the first run was extended into the warm gap: %d bytes of [16Ki, 24Ki) carved", n)
	}
	for _, r := range [][2]int64{{8 << 10, 16 << 10}, {24 << 10, 32 << 10}} {
		if n := carvedBytesIn(sink.fakeSink, r[0], r[1]); n != r[1]-r[0] {
			t.Fatalf("run [%d, %d) carved %d bytes, want %d", r[0], r[1], n, r[1]-r[0])
		}
	}
}
