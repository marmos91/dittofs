package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

// corruptFirstPayloadByte flips a single payload byte of id's first interval
// directly in the backing segment file, simulating on-disk corruption of a valid
// record between recovery and a warm read. It writes through a fresh fd; the
// store's own fd sees the change via the shared page cache.
func corruptFirstPayloadByte(t *testing.T, s *Store, id FileID) {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	fi := sh.index[id]
	if fi == nil || len(fi.ivs) == 0 {
		sh.mu.Unlock()
		t.Fatalf("no interval for %q to corrupt", id)
	}
	iv := fi.ivs[0]
	segID := iv.loc.SegmentID
	off := iv.loc.Offset // first payload byte of the interval
	sh.mu.Unlock()

	f, err := os.OpenFile(s.segPath(segID), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatalf("read payload byte: %v", err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatalf("write corrupt byte: %v", err)
	}
}

// TestVerifiedReadDetectsCorruption asserts that with VerifyReads on, a warm read
// of a record whose on-disk payload was corrupted after write fails closed with a
// *CorruptRangeError — never silent bytes, never a cold zero-fill.
func TestVerifiedReadDetectsCorruption(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{})
	s.SetVerifyReads(true)

	payload := bytes.Repeat([]byte("dittofs-verify-"), 512) // ~7.5 KiB
	if err := s.WriteAt(ctx, "f1", 0, payload); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Baseline: a verified read of the intact record round-trips.
	got := make([]byte, len(payload))
	if _, cold, err := s.ReadAt(ctx, "f1", 0, got); err != nil || cold {
		t.Fatalf("baseline verified read: err=%v cold=%v", err, cold)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("baseline verified read mismatch")
	}

	corruptFirstPayloadByte(t, s, "f1")

	clear(got)
	_, cold, err := s.ReadAt(ctx, "f1", 0, got)
	var cre *CorruptRangeError
	if !errors.As(err, &cre) {
		t.Fatalf("want *CorruptRangeError, got err=%v", err)
	}
	if cold {
		t.Fatalf("a corrupt range must never be reported cold (cold zero-fills)")
	}
	if cre.FileID != "f1" {
		t.Fatalf("CorruptRangeError names wrong file: %+v", cre)
	}
}

// TestVerifiedReadDetectsFileIDCorruption asserts that a flipped FileID byte is
// caught even though neither the header CRC nor the payload CRC covers the FileID
// bytes. Without the record-belongs-to-this-file check the read would pass CRC
// and hand back the wrong file's payload silently.
func TestVerifiedReadDetectsFileIDCorruption(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{})
	s.SetVerifyReads(true)

	payload := bytes.Repeat([]byte("dittofs-verify-"), 512)
	if err := s.WriteAt(ctx, "f1", 0, payload); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Flip a byte inside the record's FileID field (right after the header),
	// which the CRCs do not cover.
	sh := s.shardFor("f1")
	sh.mu.Lock()
	fi := sh.index["f1"]
	iv := fi.ivs[0]
	segID := iv.loc.SegmentID
	fidOff := iv.recOff + recordHeaderSize // first FileID byte
	sh.mu.Unlock()

	f, err := os.OpenFile(s.segPath(segID), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], fidOff); err != nil {
		t.Fatalf("read fileID byte: %v", err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b[:], fidOff); err != nil {
		t.Fatalf("write corrupt fileID byte: %v", err)
	}
	_ = f.Close()

	got := make([]byte, len(payload))
	_, cold, err := s.ReadAt(ctx, "f1", 0, got)
	var cre *CorruptRangeError
	if !errors.As(err, &cre) {
		t.Fatalf("want *CorruptRangeError for flipped FileID, got err=%v", err)
	}
	if cold {
		t.Fatalf("a corrupt range must never be reported cold")
	}
}

// TestUnverifiedReadReturnsRawBytes documents that the default (writeback) fast
// path does NOT verify: it serves the corrupted byte verbatim with a single raw
// read and no error. That the corrupted byte leaks through is the proof the off
// path took no covering-record read (a verifying read would have CRC-failed).
func TestUnverifiedReadReturnsRawBytes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{}) // VerifyReads defaults off

	payload := bytes.Repeat([]byte("dittofs-verify-"), 512)
	if err := s.WriteAt(ctx, "f1", 0, payload); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	corruptFirstPayloadByte(t, s, "f1")

	got := make([]byte, len(payload))
	n, cold, err := s.ReadAt(ctx, "f1", 0, got)
	if err != nil || cold || n != len(payload) {
		t.Fatalf("raw read: err=%v cold=%v n=%d", err, cold, n)
	}
	if bytes.Equal(got, payload) {
		t.Fatalf("expected the corrupted byte to leak through the unverified fast path")
	}
}
