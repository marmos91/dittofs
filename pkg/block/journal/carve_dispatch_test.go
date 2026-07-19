package journal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

var errCtrlCommit = errors.New("ctrl sink: forced commit failure")

// ctrlSink is a BlockSink that parks each CommitBlock on a per-block gate so a
// test can observe and control when uploads complete. It counts in-flight commits
// (to bound concurrency), can fail a chosen block, and marks committed hashes
// durable on the paired deduper. It deliberately does NOT implement
// supersededReaper, so carve skips the manifest reap.
type ctrlSink struct {
	dedup *fakeDeduper

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	blocks      int
	openAll     bool                    // once set, commits no longer park on a gate
	gates       map[int64]chan struct{} // block first-offset -> release gate
	failOff     int64                   // first-offset of the block to fail; -1 = none

	arrived chan int64 // first-offset of each block as its CommitBlock starts
}

func newCtrlSink(d *fakeDeduper) *ctrlSink {
	return &ctrlSink{
		dedup:   d,
		gates:   map[int64]chan struct{}{},
		failOff: -1,
		arrived: make(chan int64, 512),
	}
}

func (s *ctrlSink) CommitBlock(_ context.Context, chunks []CarveChunk) error {
	off := chunks[0].FileOffset
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.maxInFlight {
		s.maxInFlight = s.inFlight
	}
	var gate chan struct{}
	if !s.openAll {
		gate = make(chan struct{})
		s.gates[off] = gate
	}
	s.mu.Unlock()

	s.arrived <- off
	if gate != nil {
		<-gate
	}

	s.mu.Lock()
	s.inFlight--
	fail := off == s.failOff
	if !fail {
		s.blocks++
		for _, c := range chunks {
			s.dedup.markDurable(c.Hash)
		}
	}
	s.mu.Unlock()
	if fail {
		return errCtrlCommit
	}
	return nil
}

// release unblocks one parked block by its first-offset.
func (s *ctrlSink) release(off int64) {
	s.mu.Lock()
	g := s.gates[off]
	delete(s.gates, off)
	s.mu.Unlock()
	if g != nil {
		close(g)
	}
}

// releaseAll unblocks every parked block and lets any later commit run ungated.
func (s *ctrlSink) releaseAll() {
	s.mu.Lock()
	s.openAll = true
	gs := make([]chan struct{}, 0, len(s.gates))
	for off, g := range s.gates {
		gs = append(gs, g)
		delete(s.gates, off)
	}
	s.mu.Unlock()
	for _, g := range gs {
		close(g)
	}
}

func (s *ctrlSink) peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFlight
}

// ctrlStore wires a store with a ctrlSink and returns both.
func ctrlStore(t *testing.T, window int) (*Store, *ctrlSink) {
	t.Helper()
	cfg := Config{
		CarveBlockSize:         32 << 10,
		CarveUploadConcurrency: window,
		ChunkParams:            chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	}
	s, err := Open(t.TempDir(), cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dd := newFakeDeduper()
	sink := newCtrlSink(dd)
	s.SetCarveTargets(dd, sink)
	return s, sink
}

// writeAdjacent lays down nWrites adjacent chunkBytes-sized writes so the file is
// one contiguous run of many intervals — the granularity flipUpTo advances at, so
// a mid-run watermark flips a subset of intervals (a single big write would be one
// interval that only flips at run end).
func writeAdjacent(t *testing.T, s *Store, id FileID, nWrites, chunkBytes int) []byte {
	t.Helper()
	ctx := context.Background()
	data := make([]byte, 0, nWrites*chunkBytes)
	for i := 0; i < nWrites; i++ {
		b := randBytes(chunkBytes, int64(i)+1)
		if err := s.WriteAt(ctx, id, int64(i*chunkBytes), b); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
		data = append(data, b...)
	}
	return data
}

func carveAsync(s *Store) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := s.Carve(context.Background(), CarveOptions{Force: true})
		ch <- err
	}()
	return ch
}

func drainArrivals(sink *ctrlSink, quiet time.Duration) []int64 {
	var offs []int64
	for {
		select {
		case off := <-sink.arrived:
			offs = append(offs, off)
		case <-time.After(quiet):
			return offs
		}
	}
}

func minOffset(offs []int64) int64 {
	m := offs[0]
	for _, o := range offs[1:] {
		if o < m {
			m = o
		}
	}
	return m
}

// TestCarveConcurrencyBounded proves successive blocks of one file commit
// concurrently up to the window, and never beyond it.
func TestCarveConcurrencyBounded(t *testing.T) {
	const window = 4
	s, sink := ctrlStore(t, window)
	// 512 KiB across 4 KiB writes -> ~16 blocks at CarveBlockSize 32 KiB, well
	// above the window so the bound is actually exercised.
	writeAdjacent(t, s, "f", 128, 4<<10)

	done := carveAsync(s)

	// Exactly window commits should start and then block: drain window arrivals,
	// then assert no (window+1)th commit is in flight while all window are held.
	for i := 0; i < window; i++ {
		select {
		case <-sink.arrived:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d commits reached the sink", i, window)
		}
	}
	select {
	case off := <-sink.arrived:
		t.Fatalf("more than window=%d commits in flight (extra at off %d)", window, off)
	case <-time.After(200 * time.Millisecond):
	}

	sink.releaseAll()
	if err := <-done; err != nil {
		t.Fatalf("carve: %v", err)
	}
	if got := sink.peak(); got != window {
		t.Fatalf("peak in-flight commits = %d, want exactly the window %d", got, window)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
}

// TestCarveFlipsInWatermarkOrder proves a later block whose upload finishes first
// cannot flip ahead of an earlier, still-uploading block: while the first block is
// held, no record flips even though every later block has committed.
func TestCarveFlipsInWatermarkOrder(t *testing.T) {
	const window = 64 // >= block count, so every block is in flight at once
	s, sink := ctrlStore(t, window)
	data := writeAdjacent(t, s, "f", 128, 4<<10)
	full := int64(len(data))

	done := carveAsync(s)

	offs := drainArrivals(sink, 300*time.Millisecond)
	if len(offs) < 2 {
		t.Fatalf("expected multiple blocks in flight, got %d", len(offs))
	}
	first := minOffset(offs)

	// Release every block except the first (lowest watermark). They commit, but
	// the ordered flip gate holds their flips behind the first block.
	for _, off := range offs {
		if off != first {
			sink.release(off)
		}
	}
	// Give the freed commits time to finish; nothing may flip yet.
	time.Sleep(200 * time.Millisecond)
	if u := s.UnsyncedBytes(); u != full {
		t.Fatalf("records flipped before the earliest block committed: unsynced=%d want %d", u, full)
	}

	// Releasing the first block lets the whole chain flip in order.
	sink.release(first)
	if err := <-done; err != nil {
		t.Fatalf("carve: %v", err)
	}
	if u := s.UnsyncedBytes(); u != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", u)
	}
	if f := recRawFlags(t, s, "f", 0); f&flagSynced == 0 {
		t.Fatalf("record not flipped synced on disk: flags=%#x", f)
	}
}

// TestCarveConcurrentCommitErrorStopsWatermark proves a mid-run commit failure
// surfaces the error and stops the watermark: blocks before the failure flip,
// the failed block and everything after it stay dirty.
func TestCarveConcurrentCommitErrorStopsWatermark(t *testing.T) {
	const window = 64
	s, sink := ctrlStore(t, window)
	data := writeAdjacent(t, s, "f", 128, 4<<10)
	full := int64(len(data))

	done := carveAsync(s)

	offs := drainArrivals(sink, 300*time.Millisecond)
	if len(offs) < 3 {
		t.Fatalf("need at least 3 blocks to fail a middle one, got %d", len(offs))
	}
	// Fail the second block (by watermark order). The first block must still flip;
	// the failed one and its successors must not.
	sorted := append([]int64(nil), offs...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	failAt := sorted[1]
	sink.mu.Lock()
	sink.failOff = failAt
	sink.mu.Unlock()

	sink.releaseAll()
	err := <-done
	if !errors.Is(err, errCtrlCommit) {
		t.Fatalf("carve returned %v, want the commit failure", err)
	}
	// The first block flipped (watermark advanced), but the failed block stopped
	// it: some dirty bytes remain.
	if u := s.UnsyncedBytes(); u == 0 || u >= full {
		t.Fatalf("watermark did not stop at the failed block: unsynced=%d full=%d", u, full)
	}
	// A record before the failure flipped; the failed block's own records must not
	// — its bytes never reached the remote.
	if f := recRawFlags(t, s, "f", 0); f&flagSynced == 0 {
		t.Fatalf("record before the failure did not flip: flags=%#x", f)
	}
	if f := recRawFlags(t, s, "f", failAt); f&flagSynced != 0 {
		t.Fatalf("failed block's record flipped synced: off=%d flags=%#x", failAt, f)
	}
}
