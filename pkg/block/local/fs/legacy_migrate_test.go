package fs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

// legacyRec is one (file_offset, payload) append-log record for a fixture.
type legacyRec struct {
	off     uint64
	payload []byte
}

// writeLegacyLog fabricates a real v0.26.0 append log at path: a 64-byte header
// (with flags, so LogFlagCompacted can be set) followed by framed records in
// the exact on-disk format the reader parses.
func writeLegacyLog(t *testing.T, path string, flags uint32, recs []legacyRec) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	tbl := crc32.MakeTable(crc32.Castagnoli)

	var buf bytes.Buffer
	hdr := make([]byte, legacyLogHeaderSize)
	copy(hdr[0:4], legacyLogMagic[:])
	binary.LittleEndian.PutUint32(hdr[4:8], legacyLogVersion)
	binary.LittleEndian.PutUint64(hdr[8:16], legacyLogHeaderSize) // RollupOffset
	binary.LittleEndian.PutUint32(hdr[16:20], flags)
	binary.LittleEndian.PutUint32(hdr[28:32], crc32.Checksum(hdr[0:28], tbl))
	buf.Write(hdr)

	for _, r := range recs {
		frame := make([]byte, legacyRecordFrameOverhead)
		binary.LittleEndian.PutUint32(frame[0:4], uint32(len(r.payload)))
		binary.LittleEndian.PutUint64(frame[4:12], r.off)
		var offBuf [8]byte
		binary.LittleEndian.PutUint64(offBuf[:], r.off)
		c := crc32.Update(0, tbl, offBuf[:])
		c = crc32.Update(c, tbl, r.payload)
		binary.LittleEndian.PutUint32(frame[12:16], c)
		buf.Write(frame)
		buf.Write(r.payload)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// allZero reports whether b is entirely zero bytes.
func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// journalRead reads directly from the journal, bypassing the ReadAt fault-in,
// so a test can observe what the journal holds independently of migration.
func journalRead(t *testing.T, s *FSStore, payloadID string, size int) []byte {
	t.Helper()
	out := make([]byte, size)
	if _, _, err := s.Store.ReadAt(context.Background(), journal.FileID(payloadID), 0, out); err != nil {
		t.Fatalf("journal ReadAt(%s): %v", payloadID, err)
	}
	return out
}

// TestLegacyLocalOnly_MigratesFaultsInAndFinishes covers the whole async path:
// open returns without materializing (non-blocking), a read faults its payload
// in (never zeros), a background-style drain materializes the rest, the
// archived legacy dirs are deleted, and a re-open is a clean idempotent open
// that still serves the migrated bytes.
func TestLegacyLocalOnly_MigratesFaultsInAndFinishes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// payload1: overlapping records prove last-write-wins ("hello world" then
	// "HELLO" over the head => "HELLO world"), plus a gap+tail prove sparseness.
	p1 := "share/file.bin"
	writeLegacyLog(t, filepath.Join(dir, "logs", p1+".log"), 0, []legacyRec{
		{off: 0, payload: []byte("hello world")},
		{off: 0, payload: []byte("HELLO")},
		{off: 32, payload: []byte("tail")},
	})
	want1 := make([]byte, 36)
	copy(want1[0:], "HELLO world")
	copy(want1[32:], "tail")

	// payload2 is never read during migration; it is drained by the background
	// loop and must still come back correct.
	p2 := "share/nested/other.bin"
	writeLegacyLog(t, filepath.Join(dir, "logs", p2+".log"), 0, []legacyRec{
		{off: 0, payload: []byte("second payload contents")},
	})
	want2 := []byte("second payload contents")

	// A non-empty (redundant) blobs/ dir must be ignored, not block migration.
	writeFile(t, filepath.Join(dir, "blobs", "0000000000000000.blob"), 2048)

	s, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{MigrateLegacyLocalOnly: true})
	if err != nil {
		t.Fatalf("NewWithOptions(MigrateLegacyLocalOnly): %v", err)
	}
	if !s.MigratedFromLegacyLocalOnly() {
		t.Fatal("MigratedFromLegacyLocalOnly = false, want true")
	}

	// Non-blocking proof: open returned with both payloads still pending and
	// NOTHING written into the journal yet (a direct journal read is all zeros).
	if got := len(s.LegacyPendingPayloads()); got != 2 {
		t.Fatalf("LegacyPendingPayloads = %d, want 2 (open must not drain)", got)
	}
	if b := journalRead(t, s, p1, len(want1)); !allZero(b) {
		t.Fatalf("journal already holds p1 bytes at open; migration was not deferred: %q", b)
	}

	// Fault-in: a read of p1 materializes just that payload and returns the real
	// bytes (would be zeros before the fix).
	got1 := make([]byte, len(want1))
	n, cold, err := s.ReadAt(ctx, p1, 0, got1)
	if err != nil {
		t.Fatalf("ReadAt(p1): %v", err)
	}
	if cold {
		t.Fatal("ReadAt(p1) reported cold on a local-only store")
	}
	if n < len(want1) || !bytes.Equal(got1, want1) {
		t.Fatalf("ReadAt(p1) = %q (n=%d), want %q", got1, n, want1)
	}
	// p2 must still be untouched in the journal (fault-in is per-payload only).
	if b := journalRead(t, s, p2, len(want2)); !allZero(b) {
		t.Fatalf("reading p1 materialized p2 too; fault-in is not per-payload: %q", b)
	}

	// Background-style drain: materialize every pending payload, then finish.
	for _, pid := range s.LegacyPendingPayloads() {
		if err := s.MaterializeLegacyPayload(pid); err != nil {
			t.Fatalf("MaterializeLegacyPayload(%s): %v", pid, err)
		}
	}
	if b := journalRead(t, s, p2, len(want2)); !bytes.Equal(b, want2) {
		t.Fatalf("after drain p2 = %q, want %q", b, want2)
	}
	if err := s.FinishLegacyMigration(); err != nil {
		t.Fatalf("FinishLegacyMigration: %v", err)
	}

	// Archived legacy dirs are deleted after the drain.
	for _, sub := range []string{"logs", "blobs"} {
		if _, err := os.Stat(filepath.Join(dir, sub+legacyBackupSuffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("archived %s%s still present after finish (err=%v)", sub, legacyBackupSuffix, err)
		}
	}
	_ = s.Close()

	// Idempotent re-open: no legacy layout remains, so it opens clean and still
	// serves the migrated bytes from the journal.
	s2, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{MigrateLegacyLocalOnly: true})
	if err != nil {
		t.Fatalf("second NewWithOptions: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if s2.MigratedFromLegacyLocalOnly() {
		t.Fatal("second open reports a migration; not idempotent")
	}
	if b := journalRead(t, s2, p1, len(want1)); !bytes.Equal(b, want1) {
		t.Fatalf("after re-open p1 = %q, want %q", b, want1)
	}
}

// TestLegacyLocalOnly_ConcurrentReadAndDrain runs many faulting reads against
// the background drain for the same payload; the shared sync.Once must let
// exactly one materialize win and every reader observe the real bytes. Run
// under -race to exercise the fault-in/drain interleaving.
func TestLegacyLocalOnly_ConcurrentReadAndDrain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pid := "share/race.bin"
	writeLegacyLog(t, filepath.Join(dir, "logs", pid+".log"), 0, []legacyRec{
		{off: 0, payload: bytes.Repeat([]byte("ABCD"), 4096)},
	})
	want := bytes.Repeat([]byte("ABCD"), 4096)

	s, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{MigrateLegacyLocalOnly: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	done := make(chan struct{})
	go func() { // background drain
		defer close(done)
		for _, p := range s.LegacyPendingPayloads() {
			_ = s.MaterializeLegacyPayload(p)
		}
	}()
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			got := make([]byte, len(want))
			if _, _, rerr := s.ReadAt(ctx, pid, 0, got); rerr != nil {
				errs <- rerr
				return
			}
			if !bytes.Equal(got, want) {
				errs <- errors.New("concurrent read returned wrong bytes")
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	<-done
}

// TestLegacyLocalOnly_RefusesCompacted proves the safety gate: a compacted log
// means some bytes live only in the unrecoverable blobs, so the open refuses
// (guardrail) and leaves every legacy byte on disk untouched.
func TestLegacyLocalOnly_RefusesCompacted(t *testing.T) {
	dir := t.TempDir()
	writeLegacyLog(t, filepath.Join(dir, "logs", "share/f.log"), legacyLogFlagCompacted, []legacyRec{
		{off: 0, payload: []byte("survivor")},
	})
	writeFile(t, filepath.Join(dir, "blobs", "0000000000000000.blob"), 1024)

	_, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{MigrateLegacyLocalOnly: true})
	if !errors.Is(err, ErrLegacyLocalFormat) {
		t.Fatalf("open over compacted legacy layout: got %v, want ErrLegacyLocalFormat", err)
	}
	// Nothing archived, nothing deleted — bytes preserved on disk.
	for _, sub := range []string{"logs", "blobs"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Fatalf("legacy %s/ was disturbed on refusal: %v", sub, err)
		}
		if _, err := os.Stat(filepath.Join(dir, sub+legacyBackupSuffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refusal created an archive for %s; it must not touch disk", sub)
		}
	}
}

// TestLegacyLocalOnly_ResumesAfterCrash proves crash-safety: a run that
// archived the dirs but never finished resumes on the next open and still
// serves the bytes; the legacy data is untouched until the drain completes.
func TestLegacyLocalOnly_ResumesAfterCrash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pid := "share/resume.bin"
	writeLegacyLog(t, filepath.Join(dir, "logs", pid+".log"), 0, []legacyRec{
		{off: 0, payload: []byte("resume me")},
	})
	want := []byte("resume me")

	// First open archives the dirs, then we "crash" (close) without finishing.
	s1, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{MigrateLegacyLocalOnly: true})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if !s1.MigratedFromLegacyLocalOnly() {
		t.Fatal("first open did not start a migration")
	}
	if _, err := os.Stat(filepath.Join(dir, "logs"+legacyBackupSuffix)); err != nil {
		t.Fatalf("archive not created on first open: %v", err)
	}
	_ = s1.Close()

	// Second open resumes from the archive.
	s2, err := NewWithOptions(dir, 1<<30, nil, FSStoreOptions{MigrateLegacyLocalOnly: true})
	if err != nil {
		t.Fatalf("resume open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.MigratedFromLegacyLocalOnly() {
		t.Fatal("resume open did not detect the incomplete migration")
	}
	got := make([]byte, len(want))
	if _, _, err := s2.ReadAt(ctx, pid, 0, got); err != nil {
		t.Fatalf("resume ReadAt: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("resume ReadAt = %q, want %q", got, want)
	}
	for _, pid := range s2.LegacyPendingPayloads() {
		if err := s2.MaterializeLegacyPayload(pid); err != nil {
			t.Fatalf("resume materialize: %v", err)
		}
	}
	if err := s2.FinishLegacyMigration(); err != nil {
		t.Fatalf("resume finish: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs"+legacyBackupSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive not cleaned after resume finish: %v", err)
	}
}
