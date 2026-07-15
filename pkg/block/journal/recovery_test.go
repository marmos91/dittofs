package journal

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// reopen closes s and opens a fresh Store over the same directory, exercising
// the recovery path.
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	dir := s.dir
	cfg := s.cfg
	clock := s.clock
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r, err := Open(dir, cfg, newFakeRemote(), clock)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func readAll(t *testing.T, s *Store, id FileID, n int) []byte {
	t.Helper()
	got := make([]byte, n)
	if _, _, err := s.ReadAt(context.Background(), id, 0, got); err != nil {
		t.Fatalf("ReadAt %s: %v", id, err)
	}
	return got
}

// TestRecoveryRoundTrip writes across several files and shards, reopens, and
// asserts every byte survives recovery unchanged.
func TestRecoveryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{ShardCount: 4}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	want := map[FileID][]byte{
		"alpha":   bytes.Repeat([]byte("A"), 5000),
		"beta":    bytes.Repeat([]byte("B"), 12345),
		"gamma":   []byte("short"),
		"delta-x": bytes.Repeat([]byte("D"), 40000),
	}
	for id, data := range want {
		if err := s.WriteAt(ctx, id, 0, data); err != nil {
			t.Fatalf("WriteAt %s: %v", id, err)
		}
	}
	// Overwrite a middle range so newest-wins must survive replay too.
	copy(want["alpha"][1000:1004], []byte("ZZZZ"))
	if err := s.WriteAt(ctx, "alpha", 1000, []byte("ZZZZ")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if err := s.Commit(ctx, "alpha"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	r := reopen(t, s)
	for id, data := range want {
		if got := readAll(t, r, id, len(data)); !bytes.Equal(got, data) {
			t.Fatalf("file %s mismatch after recovery", id)
		}
	}
}

// segTail returns the on-disk size of a segment file.
func segTail(t *testing.T, s *Store, id uint64) int64 {
	t.Helper()
	fi, err := os.Stat(s.segPath(id))
	if err != nil {
		t.Fatalf("stat seg %d: %v", id, err)
	}
	return fi.Size()
}

// TestTornWriteRecoveryLSL06 appends a valid prefix, then a torn (truncated
// payload) final record, and asserts recovery drops the torn tail, keeps the
// prefix, and truncates the segment at the boundary. Single shard keeps every
// record in segment 0.
func TestTornWriteRecoveryLSL06(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	good := bytes.Repeat([]byte("valid-prefix-"), 100)
	if err := s.WriteAt(ctx, "f", 0, good); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, "f"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Boundary offset after the last good record.
	cleanTail := s.shards[0].active.tail.Load()

	// Hand-append a torn record: a valid header (correct header CRC) followed by
	// only part of its declared payload — exactly a crash mid-append.
	seg := s.shards[0].active
	fileID := []byte("f")
	hdr := encodeHeader(recordHeader{
		FileIDLen:  uint16(len(fileID)),
		FileOffset: 9999,
		PayloadLen: 4096, // claims 4096 bytes...
		Version:    999,
	}, fileID)
	if _, err := seg.fd.WriteAt(hdr, cleanTail); err != nil {
		t.Fatalf("write torn header: %v", err)
	}
	// ...but only 10 payload bytes actually reach disk.
	if _, err := seg.fd.WriteAt(bytes.Repeat([]byte("x"), 10), cleanTail+int64(len(hdr))); err != nil {
		t.Fatalf("write torn payload: %v", err)
	}
	_ = seg.fd.Sync()

	r := reopen(t, s)
	if got := readAll(t, r, "f", len(good)); !bytes.Equal(got, good) {
		t.Fatalf("valid prefix corrupted after torn recovery")
	}
	// The torn tail was truncated: segment size is back to the clean boundary.
	if sz := segTail(t, r, 0); sz != cleanTail {
		t.Fatalf("segment not truncated at torn boundary: size=%d want=%d", sz, cleanTail)
	}
}

// TestCRCCoincidenceTornRecordGuarded writes a torn record whose header CRC is
// perfectly valid but whose PayloadLen is absurd. The PayloadLen ceiling must
// reject it before any allocation, so recovery treats it as the torn boundary.
func TestCRCCoincidenceTornRecordGuarded(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	good := []byte("keep-me")
	if err := s.WriteAt(ctx, "f", 0, good); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, "f"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	cleanTail := s.shards[0].active.tail.Load()

	fileID := []byte("f")
	// Header CRC is valid (encodeHeader computes it), PayloadLen is way over the
	// SegmentSize ceiling — the classic CRC-coincidence torn tail.
	hdr := encodeHeader(recordHeader{
		FileIDLen:  uint16(len(fileID)),
		PayloadLen: uint32(s.cfg.SegmentSize) + 1<<20,
		Version:    12345,
	}, fileID)
	if _, err := s.shards[0].active.fd.WriteAt(hdr, cleanTail); err != nil {
		t.Fatalf("write bogus header: %v", err)
	}
	_ = s.shards[0].active.fd.Sync()

	r := reopen(t, s)
	if got := readAll(t, r, "f", len(good)); !bytes.Equal(got, good) {
		t.Fatalf("valid prefix corrupted")
	}
	if sz := segTail(t, r, 0); sz != cleanTail {
		t.Fatalf("bogus record not truncated: size=%d want=%d", sz, cleanTail)
	}
}

// TestVersionLSNMonotonicAfterReopen asserts the global LSN resumes strictly
// past every replayed record and never reissues an observed version.
func TestVersionLSNMonotonicAfterReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.WriteAt(ctx, "f", int64(i*10), []byte("data")); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	}
	maxObserved := s.version.Load() // last issued version

	r := reopen(t, s)
	// Next write must get a version strictly greater than any observed before.
	if err := r.WriteAt(ctx, "f", 100, []byte("post")); err != nil {
		t.Fatalf("post-reopen WriteAt: %v", err)
	}
	if got := r.version.Load(); got <= maxObserved {
		t.Fatalf("LSN not monotonic: post-reopen version %d <= observed %d", got, maxObserved)
	}
}

// TestMissingIdxRebuilt deletes a sealed segment's .idx sidecar, then asserts
// recovery rebuilds it, warns, and serves the data intact.
func TestMissingIdxRebuilt(t *testing.T) {
	dir := t.TempDir()
	// Small segment + single shard forces a seal after ~1 MiB of writes.
	s, err := Open(dir, Config{ShardCount: 1, SegmentSize: minSegmentSize}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	chunk := bytes.Repeat([]byte("payload-"), 8192) // 64 KiB
	var written int
	for off := 0; off < int(minSegmentSize)+128<<10; off += len(chunk) {
		if err := s.WriteAt(ctx, "big", int64(off), chunk); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		written = off + len(chunk)
	}
	if err := s.Commit(ctx, "big"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Segment 0 should now be sealed with an .idx sidecar.
	if len(s.shards[0].sealed) == 0 {
		t.Fatalf("expected at least one sealed segment")
	}
	if err := os.Remove(s.idxPath(0)); err != nil {
		t.Fatalf("remove sealed .idx: %v", err)
	}

	var warned bool
	prev := logf
	logf = func(format string, args ...any) { warned = true }
	defer func() { logf = prev }()

	r := reopen(t, s)
	if !warned {
		t.Fatalf("expected a Warn for the missing .idx")
	}
	if _, err := os.Stat(r.idxPath(0)); err != nil {
		t.Fatalf("sealed .idx not rebuilt: %v", err)
	}
	// Data still intact across the sealed + active segments.
	got := make([]byte, len(chunk))
	if _, _, err := r.ReadAt(ctx, "big", 0, got); err != nil {
		t.Fatalf("ReadAt after rebuild: %v", err)
	}
	if !bytes.Equal(got, chunk) {
		t.Fatalf("data corrupted after .idx rebuild")
	}
	_ = written
}

// fixedClock is a Clock pinned to a fixed instant, used to drive the age gate.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// TestOrphanSweepAgeGated asserts an unattachable, aged segment file is
// unlinked on reopen, while the store's real data is untouched.
func TestOrphanSweepAgeGated(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, []byte("real-data")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, "f"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Drop a stray, empty, valid but excess segment file with an old header.
	orphanID := uint64(9999)
	old := time.Now().Add(-time.Hour)
	if err := os.WriteFile(s.segPath(orphanID), encodeSegHeader(orphanID, old, 0), 0o644); err != nil {
		t.Fatalf("write orphan seg: %v", err)
	}
	// Backdate its mtime past the age gate.
	if err := os.Chtimes(s.segPath(orphanID), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	r, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), fixedClock{t: time.Now()})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = r.Close() }()

	if _, err := os.Stat(r.segPath(orphanID)); !os.IsNotExist(err) {
		t.Fatalf("aged orphan segment not swept: err=%v", err)
	}
	if got := readAll(t, r, "f", len("real-data")); string(got) != "real-data" {
		t.Fatalf("real data lost during orphan sweep: %q", got)
	}
}

// TestOrphanSweepSparesYoung asserts a fresh (young) orphan is left in place.
func TestOrphanSweepSparesYoung(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.WriteAt(context.Background(), "f", 0, []byte("x")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A stray segment that is unreadable garbage but freshly written stays put.
	orphanID := uint64(4242)
	if err := os.WriteFile(s.segPath(orphanID), []byte("not a segment header at all!!!"), 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	r, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = r.Close() }()
	if _, err := os.Stat(s.segPath(orphanID)); err != nil {
		t.Fatalf("young orphan should be spared, got: %v", err)
	}
}

// TestConcurrentReadWriteAfterRecovery exercises the recovered store under the
// race detector with concurrent readers and writers.
func TestConcurrentReadWriteAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{ShardCount: 4}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	seed := bytes.Repeat([]byte("seed"), 1024)
	for i := 0; i < 8; i++ {
		if err := s.WriteAt(ctx, FileID([]byte{byte('a' + i)}), 0, seed); err != nil {
			t.Fatalf("seed WriteAt: %v", err)
		}
	}
	r := reopen(t, s)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		id := FileID([]byte{byte('a' + i)})
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = r.WriteAt(ctx, id, int64(len(seed)+j*4), []byte("more"))
			}
		}()
		go func() {
			defer wg.Done()
			buf := make([]byte, len(seed))
			for j := 0; j < 50; j++ {
				_, _, _ = r.ReadAt(ctx, id, 0, buf)
			}
		}()
	}
	wg.Wait()

	for i := 0; i < 8; i++ {
		id := FileID([]byte{byte('a' + i)})
		if got := readAll(t, r, id, len(seed)); !bytes.Equal(got, seed) {
			t.Fatalf("seed data corrupted for %s", id)
		}
	}
}
