package journal

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// pollUntil spins on cond until it holds or the timeout elapses. Called from the
// test goroutine only (it uses t.Fatalf).
func pollUntil(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// fakeClock is a settable Clock for the age-gate tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeDeduper models the remote-durable oracle: a hash is durable once a block
// carrying it has committed. Safe for concurrent use.
type fakeDeduper struct {
	mu      sync.Mutex
	durable map[ChunkHash]bool
}

func newFakeDeduper() *fakeDeduper { return &fakeDeduper{durable: map[ChunkHash]bool{}} }

func (d *fakeDeduper) IsChunkDurable(_ context.Context, h ChunkHash) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.durable[h], nil
}

func (d *fakeDeduper) markDurable(h ChunkHash) {
	d.mu.Lock()
	d.durable[h] = true
	d.mu.Unlock()
}

// fakeSink records every committed chunk and, on success, marks its hash durable
// in the paired deduper (so a later carve dedups it). failErr and onCommit let a
// test force a commit failure or assert store state at commit time.
type fakeSink struct {
	mu        sync.Mutex
	dedup     *fakeDeduper
	blocks    int
	chunks    map[int64][]byte // fileOffset -> plaintext
	failErr   error
	okCommits int // number of commits allowed before failErr kicks in
	onCommit  func(chunks []CarveChunk)
}

func newFakeSink(d *fakeDeduper) *fakeSink {
	return &fakeSink{dedup: d, chunks: map[int64][]byte{}}
}

func (s *fakeSink) CommitBlock(_ context.Context, chunks []CarveChunk) error {
	s.mu.Lock()
	hook := s.onCommit
	fail := s.failErr != nil && s.blocks >= s.okCommits
	failErr := s.failErr
	s.mu.Unlock()
	if hook != nil {
		hook(chunks)
	}
	if fail {
		return failErr
	}
	s.mu.Lock()
	s.blocks++
	for _, c := range chunks {
		cp := make([]byte, len(c.Data))
		copy(cp, c.Data)
		s.chunks[c.FileOffset] = cp
		s.dedup.markDurable(c.Hash)
	}
	s.mu.Unlock()
	return nil
}

// carved reassembles every committed chunk in file-offset order.
func (s *fakeSink) carved() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	offs := make([]int64, 0, len(s.chunks))
	for off := range s.chunks {
		offs = append(offs, off)
	}
	// simple insertion sort — a handful of chunks per test
	for i := 1; i < len(offs); i++ {
		for j := i; j > 0 && offs[j-1] > offs[j]; j-- {
			offs[j-1], offs[j] = offs[j], offs[j-1]
		}
	}
	var out []byte
	for _, off := range offs {
		out = append(out, s.chunks[off]...)
	}
	return out
}

func (s *fakeSink) blockCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocks
}

// carveStore opens a store with the fakes wired and a controllable clock.
func carveStore(t *testing.T, cfg Config) (*Store, *fakeDeduper, *fakeSink, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	s, err := Open(t.TempDir(), cfg, newFakeRemote(), clk)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dd := newFakeDeduper()
	sink := newFakeSink(dd)
	s.SetCarveTargets(dd, sink)
	return s, dd, sink, clk
}

func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// recRawFlags reads the on-disk Flags byte of the record covering fileOff.
func recRawFlags(t *testing.T, s *Store, id FileID, fileOff int64) uint8 {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		t.Fatalf("no index for %q", id)
	}
	for _, iv := range fi.ivs {
		if iv.fileOff <= fileOff && fileOff < iv.end() {
			seg := sh.segment(iv.loc.SegmentID)
			var b [1]byte
			if _, err := seg.fd.ReadAt(b[:], iv.recOff+recordFlagsOffset); err != nil {
				t.Fatalf("read flags: %v", err)
			}
			return b[0]
		}
	}
	t.Fatalf("no interval covering offset %d", fileOff)
	return 0
}

// recOffAt returns the SegOffset of the record backing fileOff.
func recOffAt(t *testing.T, s *Store, id FileID, fileOff int64) int64 {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		t.Fatalf("no index for %q", id)
	}
	for _, iv := range fi.ivs {
		if iv.fileOff <= fileOff && fileOff < iv.end() {
			return iv.recOff
		}
	}
	t.Fatalf("no interval covering offset %d", fileOff)
	return 0
}

// forceDirty simulates a crash between commit and flip: the records go back to
// synced=false (on disk and in memory) while the deduper keeps the committed
// hashes, so a re-carve must dedup to a no-op.
func forceDirty(t *testing.T, s *Store, id FileID) {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	for k := range fi.ivs {
		iv := fi.ivs[k]
		seg := sh.segment(iv.loc.SegmentID)
		if _, err := seg.fd.WriteAt([]byte{0}, iv.recOff+recordFlagsOffset); err != nil {
			t.Fatalf("clear flag: %v", err)
		}
		if fi.ivs[k].synced {
			fi.ivs[k].synced = false
			s.unsynced.Add(fi.ivs[k].length)
		}
	}
	fi.firstDirtyNanos = s.clock.Now().UnixNano()
}

func TestCarveRoundTripAndFlip(t *testing.T) {
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 1 << 20})
	ctx := context.Background()

	data := randBytes(3<<20, 1) // ~3 MiB -> several chunks -> multiple blocks
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if s.UnsyncedBytes() != int64(len(data)) {
		t.Fatalf("pre-carve unsynced=%d want %d", s.UnsyncedBytes(), len(data))
	}

	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if res.BlocksWritten < 1 || res.BytesCarved != int64(len(data)) {
		t.Fatalf("result=%+v want blocks>=1 bytes=%d", res, len(data))
	}
	if got := sink.carved(); string(got) != string(data) {
		t.Fatalf("carved bytes mismatch: got %d bytes", len(got))
	}
	// Records are now synced: counter drained and the on-disk flag is set.
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
	if f := recRawFlags(t, s, "f", 0); f&flagSynced == 0 {
		t.Fatalf("record not flipped synced on disk: flags=%#x", f)
	}
	// Warm read still returns the data unchanged.
	got := make([]byte, len(data))
	if _, cold, err := s.ReadAt(ctx, "f", 0, got); err != nil || cold {
		t.Fatalf("ReadAt: err=%v cold=%v", err, cold)
	}
	if string(got) != string(data) {
		t.Fatalf("warm read mismatch after carve")
	}
}

func TestCarveCommitStrictlyBeforeFlip(t *testing.T) {
	// One sub-CarveBlockSize file -> exactly one block, flushed at run end. At the
	// moment CommitBlock runs, nothing may be flipped yet.
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 8 << 20})
	ctx := context.Background()
	data := randBytes(512<<10, 2)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}

	var checked bool
	sink.onCommit = func(chunks []CarveChunk) {
		checked = true
		if s.UnsyncedBytes() != int64(len(data)) {
			t.Errorf("flip happened before commit: unsynced=%d want %d", s.UnsyncedBytes(), len(data))
		}
		if f := recRawFlags(t, s, "f", 0); f&flagSynced != 0 {
			t.Errorf("record already flipped at commit time: flags=%#x", f)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if !checked {
		t.Fatalf("commit hook never ran")
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
}

func TestCarveSinkErrorLeavesDirty(t *testing.T) {
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 1 << 20})
	ctx := context.Background()
	data := randBytes(1<<20, 3)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}
	sink.failErr = errors.New("boom")

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err == nil {
		t.Fatalf("expected carve error from failing sink")
	}
	if s.UnsyncedBytes() != int64(len(data)) {
		t.Fatalf("failed carve changed unsynced=%d want %d", s.UnsyncedBytes(), len(data))
	}
	if f := recRawFlags(t, s, "f", 0); f&flagSynced != 0 {
		t.Fatalf("record flipped despite commit failure: flags=%#x", f)
	}
}

func TestCarveDedupReCarveIsNoOp(t *testing.T) {
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 1 << 20})
	ctx := context.Background()
	data := randBytes(2<<20, 4)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("first carve: %v", err)
	}
	first := sink.blockCount()
	if first < 1 {
		t.Fatalf("first carve wrote no block")
	}

	// Simulate a crash between commit and flip: records dirty again, hashes still
	// durable. The re-carve must upload nothing (dedup no-op) but re-flip.
	forceDirty(t, s, "f")
	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("re-carve: %v", err)
	}
	if res.BlocksWritten != 0 || res.BytesCarved != 0 {
		t.Fatalf("re-carve was not a dedup no-op: %+v", res)
	}
	if sink.blockCount() != first {
		t.Fatalf("re-carve committed extra blocks: %d -> %d", first, sink.blockCount())
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("re-carve did not re-flip: unsynced=%d", s.UnsyncedBytes())
	}
	if f := recRawFlags(t, s, "f", 0); f&flagSynced == 0 {
		t.Fatalf("re-carve did not re-flip on disk: flags=%#x", f)
	}
}

func TestCarveRecordSplitNoPrematureFlip(t *testing.T) {
	// A newer overlapping write splits one physical record into two live fragments.
	// If only the first fragment's block commits (the second fails), the record's
	// on-disk synced bit must stay clear — flipping it while a live fragment is
	// still dirty would let a crash make recovery treat that dirty fragment as
	// synced, and its bytes would never carve (data loss).
	//
	// Pin the upload window to 1 so the two blocks commit in submission order and
	// the "first succeeds, second fails" injection (okCommits) is deterministic.
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 1 << 20, CarveUploadConcurrency: 1})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, randBytes(6<<20, 21)); err != nil { // record R = [0,6MiB)
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 1<<20, randBytes(1<<20, 22)); err != nil { // split R at [1MiB,2MiB)
		t.Fatal(err)
	}
	// Both surviving fragments must be the same physical record.
	if a, b := recOffAt(t, s, "f", 0), recOffAt(t, s, "f", 2<<20); a != b {
		t.Fatalf("fragments not from one record: %d vs %d", a, b)
	}

	// Let the first block commit, then fail — the [2MiB,6MiB) fragment stays undurable.
	sink.okCommits = 1
	sink.failErr = errors.New("second block fails")
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err == nil {
		t.Fatalf("expected carve to fail on the second block (need >=2 chunks)")
	}
	if sink.blockCount() < 1 {
		t.Fatalf("first block did not commit")
	}
	// The record must NOT be flipped: it still has a dirty live fragment.
	if f := recRawFlags(t, s, "f", 0); f&flagSynced != 0 {
		t.Fatalf("record flipped while a live fragment is still dirty: flags=%#x", f)
	}
	if s.UnsyncedBytes() == 0 {
		t.Fatalf("dirty fragment not tracked as unsynced after partial carve")
	}

	// Completing the carve now flips the record and drains the dirty bytes.
	sink.failErr = nil
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("completing carve: %v", err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("record not fully carved after retry: unsynced=%d", s.UnsyncedBytes())
	}
	if f := recRawFlags(t, s, "f", 0); f&flagSynced == 0 {
		t.Fatalf("record not flipped after full carve: flags=%#x", f)
	}
}

func TestCarveUploadConcurrencyWindow(t *testing.T) {
	// Packing is sequential, but up to CarveUploadConcurrency block uploads overlap.
	// Hold every CommitBlock inside onCommit so the window fills, then assert exactly
	// that many uploads were simultaneously in flight — never more (the sem bound).
	const window = 4
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 256 << 10, CarveUploadConcurrency: window})
	ctx := context.Background()
	// Default ~1 MiB min-chunk >= CarveBlockSize, so each chunk is its own block:
	// 8 MiB -> ~8 blocks, comfortably more than the window.
	if err := s.WriteAt(ctx, "f", 0, randBytes(8<<20, 40)); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		inflight int
		maxSeen  int
	)
	release := make(chan struct{})
	sink.onCommit = func(_ []CarveChunk) {
		mu.Lock()
		inflight++
		if inflight > maxSeen {
			maxSeen = inflight
		}
		mu.Unlock()
		<-release // hold the upload so successive blocks pile up against the window
		mu.Lock()
		inflight--
		mu.Unlock()
	}

	done := make(chan error, 1)
	go func() { _, err := s.Carve(ctx, CarveOptions{Force: true}); done <- err }()

	pollUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return maxSeen == window
	}, 5*time.Second, "upload window to fill")
	// Give any (buggy) unbounded fan-out a moment to overshoot the window.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	got := maxSeen
	mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("carve: %v", err)
	}
	if got != window {
		t.Fatalf("peak in-flight uploads = %d, want exactly the window %d", got, window)
	}
}

func TestCarveFlipsInWatermarkOrder(t *testing.T) {
	// A later block's upload finishing first must NOT let its flip jump ahead: the
	// flip is crash-safety ordering. Hold block0 uncommitted while block1 commits,
	// then assert nothing has flipped until block0 lands.
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 1 << 20, CarveUploadConcurrency: 4})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, randBytes(2<<20, 41)); err != nil {
		t.Fatal(err)
	}
	total := s.UnsyncedBytes()

	gate0 := make(chan struct{})
	sink.onCommit = func(chunks []CarveChunk) {
		if chunks[0].FileOffset == 0 {
			<-gate0 // the first block's upload finishes last
		}
	}

	done := make(chan error, 1)
	go func() { _, err := s.Carve(ctx, CarveOptions{Force: true}); done <- err }()

	// A later block commits while block0 is gated. Its flip must wait its turn, so
	// no bytes may drain until block0 is durable.
	pollUntil(t, func() bool { return sink.blockCount() >= 1 }, 5*time.Second, "a later block to commit")
	if u := s.UnsyncedBytes(); u != total {
		t.Fatalf("flip applied out of watermark order: unsynced=%d want %d (nothing may flip before block0)", u, total)
	}
	close(gate0)
	if err := <-done; err != nil {
		t.Fatalf("carve: %v", err)
	}
	if u := s.UnsyncedBytes(); u != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", u)
	}
}

func TestCarveConcurrentCommitErrorStopsWatermark(t *testing.T) {
	// The first block commits; the second's upload fails. The watermark must stop at
	// the failed block: block0's bytes drain, block1's stay dirty, and the error
	// surfaces. Two separate 1 MiB writes give two records so the frontier is exact.
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 1 << 20, CarveUploadConcurrency: 4})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, randBytes(1<<20, 42)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 1<<20, randBytes(1<<20, 43)); err != nil {
		t.Fatal(err)
	}

	sink.okCommits = 1
	sink.failErr = errors.New("second block upload fails")
	// Serialize the two commits so okCommits is deterministic under the window: the
	// non-first block only reaches the (failing) commit path after block0 committed.
	sink.onCommit = func(chunks []CarveChunk) {
		if chunks[0].FileOffset != 0 {
			for sink.blockCount() < 1 {
				time.Sleep(time.Millisecond)
			}
		}
	}

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err == nil {
		t.Fatalf("expected the second block's commit error to surface")
	}
	if u := s.UnsyncedBytes(); u != 1<<20 {
		t.Fatalf("watermark did not stop at the failed block: unsynced=%d want %d", u, 1<<20)
	}
}

func TestCarveDedupVisibilityCommitGated(t *testing.T) {
	// Block K must not dedup against block K-1's not-yet-committed hash: a durable
	// verdict on an uncommitted sibling would flip records whose bytes never reached
	// the remote. Two identical chunks land in two blocks; hold block0 uncommitted
	// while block1 packs the twin. Block1 must still upload its own block.
	const chunkSize = 64 << 10
	fixed := chunker.Params{Min: chunkSize, Avg: chunkSize, Max: chunkSize} // deterministic, twin chunks
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         chunkSize,
		CarveUploadConcurrency: 4,
		ChunkParams:            fixed,
	})
	ctx := context.Background()
	x := randBytes(chunkSize, 44)
	data := append(append([]byte{}, x...), x...) // X ++ X -> two chunks, same hash
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}

	gate0 := make(chan struct{})
	sink.onCommit = func(chunks []CarveChunk) {
		if chunks[0].FileOffset == 0 {
			<-gate0 // keep block0's hash uncommitted while block1 packs the twin
		}
	}

	done := make(chan error, 1)
	go func() { _, err := s.Carve(ctx, CarveOptions{Force: true}); done <- err }()

	// block1 commits its own block while block0 is uncommitted. If the twin hash were
	// visible as durable early, block1 would dedup it away and never reach the sink.
	pollUntil(t, func() bool { return sink.blockCount() >= 1 }, 5*time.Second, "block1 to commit while block0 gated")
	close(gate0)
	if err := <-done; err != nil {
		t.Fatalf("carve: %v", err)
	}
	if n := sink.blockCount(); n != 2 {
		t.Fatalf("committed %d blocks, want 2: block1 must not dedup on block0's uncommitted hash", n)
	}
}

func TestCarveReopenReCarveIsNoOp(t *testing.T) {
	// Full crash simulation: carve durably (deduper populated), then lose only the
	// on-disk synced flips, close, reopen, and carve again. Recovery must replay
	// the records with the correct SegOffset so the re-flip lands on the record —
	// not the segment header — and dedup must make the re-carve upload-free.
	dir := t.TempDir()
	clk := newFakeClock()
	dd := newFakeDeduper()
	data := randBytes(2<<20, 11)

	s1, err := Open(dir, Config{CarveBlockSize: 1 << 20}, newFakeRemote(), clk)
	if err != nil {
		t.Fatalf("Open s1: %v", err)
	}
	s1.SetCarveTargets(dd, newFakeSink(dd))
	ctx := context.Background()
	if err := s1.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("carve s1: %v", err)
	}
	// Simulate the flip never reaching disk (crash between commit and flip).
	forceDirty(t, s1, "f")
	_ = s1.Close()

	// Reopen: recovery rebuilds the index from the (now dirty) records.
	s2, err := Open(dir, Config{CarveBlockSize: 1 << 20}, newFakeRemote(), clk)
	if err != nil {
		t.Fatalf("Open s2: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if s2.UnsyncedBytes() != int64(len(data)) {
		t.Fatalf("recovered unsynced=%d want %d", s2.UnsyncedBytes(), len(data))
	}
	sink2 := newFakeSink(dd)
	s2.SetCarveTargets(dd, sink2)

	res, err := s2.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("re-carve after reopen: %v", err)
	}
	if res.BlocksWritten != 0 || res.BytesCarved != 0 || sink2.blockCount() != 0 {
		t.Fatalf("reopen re-carve was not a dedup no-op: %+v blocks=%d", res, sink2.blockCount())
	}
	if s2.UnsyncedBytes() != 0 {
		t.Fatalf("reopen re-carve did not re-flip: unsynced=%d", s2.UnsyncedBytes())
	}
	if f := recRawFlags(t, s2, "f", 0); f&flagSynced == 0 {
		t.Fatalf("reopen re-carve did not flip record on disk: flags=%#x", f)
	}
	// The data is intact and the segment header is unharmed (a mis-targeted flip
	// at offset 24 would corrupt it and fail the read).
	got := make([]byte, len(data))
	if _, _, err := s2.ReadAt(ctx, "f", 0, got); err != nil || string(got) != string(data) {
		t.Fatalf("data corrupted after reopen re-carve: err=%v", err)
	}
}

func TestCarveSizeTrigger(t *testing.T) {
	// Small, recent write with no Force stays put; crossing CarveBlockSize carves.
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 2 << 20, CarveMaxAge: time.Hour})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, randBytes(512<<10, 5)); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Carve(ctx, CarveOptions{}); err != nil || res.BlocksWritten != 0 {
		t.Fatalf("sub-threshold recent file should not carve: res=%+v err=%v", res, err)
	}
	if s.UnsyncedBytes() == 0 {
		t.Fatalf("sub-threshold file was carved")
	}

	// Cross the threshold; now it is eligible without Force.
	if err := s.WriteAt(ctx, "f", 512<<10, randBytes(2<<20, 6)); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Carve(ctx, CarveOptions{}); err != nil || res.BlocksWritten == 0 {
		t.Fatalf("over-threshold file should carve: res=%+v err=%v", res, err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("over-threshold carve left dirty bytes: %d", s.UnsyncedBytes())
	}
	if sink.blockCount() == 0 {
		t.Fatalf("no block committed on size trigger")
	}
}

func TestCarveAgeTrigger(t *testing.T) {
	s, _, _, clk := carveStore(t, Config{CarveBlockSize: 64 << 20, CarveMaxAge: 5 * time.Second})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, randBytes(256<<10, 7)); err != nil {
		t.Fatal(err)
	}
	// Fresh: below size, within age -> no carve.
	if res, err := s.Carve(ctx, CarveOptions{}); err != nil || res.BlocksWritten != 0 {
		t.Fatalf("fresh sub-threshold file should not carve: res=%+v err=%v", res, err)
	}
	// Age it past CarveMaxAge -> eligible.
	clk.advance(10 * time.Second)
	if res, err := s.Carve(ctx, CarveOptions{}); err != nil || res.BlocksWritten == 0 {
		t.Fatalf("aged file should carve: res=%+v err=%v", res, err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("aged carve left dirty bytes: %d", s.UnsyncedBytes())
	}
}

func TestCarveNotWired(t *testing.T) {
	s := testStore(t, Config{})
	if _, err := s.Carve(context.Background(), CarveOptions{Force: true}); !errors.Is(err, errCarveNotWired) {
		t.Fatalf("expected errCarveNotWired, got %v", err)
	}
}

func TestCarveConcurrentOverwrite(t *testing.T) {
	s, _, _, _ := carveStore(t, Config{CarveBlockSize: 1 << 20})
	ctx := context.Background()

	const size = 4 << 20
	if err := s.WriteAt(ctx, "f", 0, randBytes(size, 8)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Writer: overwrite random 64 KiB windows until told to stop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(99))
		buf := make([]byte, 64<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			off := int64(rng.Intn(size-len(buf))) &^ 0xFFF
			rng.Read(buf)
			_ = s.WriteAt(ctx, "f", off, buf)
		}
	}()
	// Carver runs its passes inline against the live writer.
	for i := 0; i < 50; i++ {
		if _, err := s.Carve(ctx, CarveOptions{FileID: "f", Force: true}); err != nil {
			t.Errorf("concurrent carve: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()

	// A final pass drains whatever the last overwrite left dirty.
	if _, err := s.Carve(ctx, CarveOptions{FileID: "f", Force: true}); err != nil {
		t.Fatalf("final carve: %v", err)
	}
	// Data must still read back at full length without error.
	got := make([]byte, size)
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt after concurrent carve: %v", err)
	}
}
