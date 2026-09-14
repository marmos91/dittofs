package engine

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// spanningReapSink fails exactly the block that carries chunks from both runs,
// commits every other block for real, and records the span the pass-end reap is
// asked about.
//
// Failing on content rather than on call order is deliberate: blocks commit
// concurrently, so "the second CommitBlock" is not a stable identity, while
// "the block that spans the boundary" is exactly the shape under test.
type spanningReapSink struct {
	inner   engineBlockSink
	gap     int64
	failErr error

	mu        sync.Mutex
	spanned   bool
	committed map[int64]int // chunk file offset -> length, committed blocks only
	reaps     [][2]int64
}

func (s *spanningReapSink) CommitBlock(ctx context.Context, chunks []CarveChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	spans := false
	first := chunks[0].FileOffset / s.gap
	for _, c := range chunks {
		if c.FileOffset/s.gap != first {
			spans = true
		}
	}
	if spans {
		s.mu.Lock()
		s.spanned = true
		s.mu.Unlock()
		return s.failErr
	}
	if err := s.inner.CommitBlock(ctx, chunks); err != nil {
		return err
	}
	s.mu.Lock()
	for _, c := range chunks {
		s.committed[c.FileOffset] = c.Size
	}
	s.mu.Unlock()
	return nil
}

func (s *spanningReapSink) ReapSupersededManifest(_ context.Context, _ journal.FileID, spans [][2]int64, _ map[int64]struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reaps = append(s.reaps, spans...)
	return nil
}

// TestFlushReapSpanStopsAtTheCommittedFrontier pins the span the pass-end reap
// may delete.
//
// When a block carrying chunks from two runs fails, the reap must be asked
// about exactly the committed prefix and nothing of the range the failed block
// held. Over-reaching past the committed frontier deletes manifest rows for
// bytes that never became durable, so a later read resolves them to zeros with
// nothing reporting a fault.
//
// It lives beside the packing rather than in the journal because the journal's
// flush seam is content-agnostic: it offers byte runs and never sees a block,
// so it cannot observe a block spanning two of them.
func TestFlushReapSpanStopsAtTheCommittedFrontier(t *testing.T) {
	ctx := context.Background()
	const (
		blockSize = 32 << 10
		recSize   = 4 << 10
		run0Recs  = 12        // [0, 48Ki): more than one block
		run1Off   = 128 << 10 // a hole keeps it a separate run
		run1Recs  = 4
	)

	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	j, err := journal.Open(t.TempDir(), journal.Config{CarveBlockSize: blockSize})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	boom := errors.New("spanning block fails")
	sink := &spanningReapSink{
		inner:     realSink(ms, remotememory.New()),
		gap:       run1Off,
		failErr:   boom,
		committed: map[int64]int{},
	}

	write := func(off int64, recs int) {
		for i := range recs {
			data := seamRandBytes(recSize, off+int64(i))
			if err := j.WriteAt(ctx, "f", off+int64(i)*recSize, data); err != nil {
				t.Fatalf("WriteAt: %v", err)
			}
		}
	}
	write(0, run0Recs)
	write(run1Off, run1Recs)

	fn, reap := newFlushClosure(j, chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
		blockSize, engineDeduper{synced: ms}, sink, nil)
	err = j.Flush(ctx, "f", journal.FlushOptions{Force: true, AfterFile: reap}, fn)
	if !errors.Is(err, boom) {
		t.Fatalf("Flush = %v, want the seeded failure", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()

	// Without a block carrying chunks from both runs the geometry does not build
	// the shape under test, and the assertion below would pass vacuously.
	if !sink.spanned {
		t.Fatal("no block carried chunks from both runs: the geometry does not build the shape under test")
	}

	var frontier int64
	for off, n := range sink.committed {
		if end := off + int64(n); off < run1Off && end > frontier {
			frontier = end
		}
	}
	if frontier == 0 || frontier >= run0Recs*recSize {
		t.Fatalf("committed frontier %d: the surviving blocks must commit a proper prefix of run 0", frontier)
	}

	want := [][2]int64{{0, frontier}}
	if !reflect.DeepEqual(sink.reaps, want) {
		t.Fatalf("reaps=%v, want %v: run 0's committed prefix, and nothing of the range the failed block held",
			sink.reaps, want)
	}
}
