package fs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tmpLog creates a fresh log file under t.TempDir() and returns its path
// and an open *os.File. The file is registered for Close via t.Cleanup for
// tests that do not call Close themselves.
func tmpLog(t *testing.T) (string, *os.File) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	f, err := initLogFile(path, time.Now().Unix())
	if err != nil {
		t.Fatalf("initLogFile: %v", err)
	}
	return path, f
}

// TestAppendLog_RoundTrip writes three records with distinct file_offsets
// and payloads, then reads them back after reopening the file and asserts
// (offset, payload) round-trip cleanly. Readback past the last record
// returns (_, _, false, nil) on clean EOF.
func TestAppendLog_RoundTrip(t *testing.T) {
	path, f := tmpLog(t)
	defer f.Close()

	type rec struct {
		off     uint64
		payload []byte
	}
	recs := []rec{
		{off: 0, payload: bytes.Repeat([]byte{0x11}, 100)},
		{off: 4096, payload: bytes.Repeat([]byte{0x22}, 200)},
		{off: 8192, payload: bytes.Repeat([]byte{0x33}, 300)},
	}

	// Seek past the header and write records.
	if _, err := f.Seek(logHeaderSize, 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if _, err := writeRecord(f, r.off, r.payload); err != nil {
			t.Fatalf("writeRecord: %v", err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}

	// Reopen read-only and verify.
	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if _, err := f2.Seek(logHeaderSize, 0); err != nil {
		t.Fatal(err)
	}
	for i, want := range recs {
		off, payload, ok, err := readRecord(f2)
		if err != nil || !ok {
			t.Fatalf("record %d: ok=%v err=%v", i, ok, err)
		}
		if off != want.off {
			t.Errorf("record %d offset: got %d want %d", i, off, want.off)
		}
		if !bytes.Equal(payload, want.payload) {
			t.Errorf("record %d payload mismatch", i)
		}
	}
	_, _, ok, readErr := readRecord(f2)
	if ok {
		t.Fatalf("expected EOF after %d records", len(recs))
	}
	if readErr != nil {
		t.Fatalf("expected nil error on clean EOF, got %v", readErr)
	}
}

// TestAppendLog_TornPayload_DetectedByCRC truncates the log mid-payload
// and asserts readRecord surfaces (0, nil, false, nil) — the recovery
// signal for "stop here and truncate" (LSL-06).
func TestAppendLog_TornPayload_DetectedByCRC(t *testing.T) {
	path, f := tmpLog(t)
	if _, err := f.Seek(logHeaderSize, 0); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0xAA}, 1024)
	if _, err := writeRecord(f, 42, payload); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Truncate mid-payload.
	if err := os.Truncate(path, st.Size()-512); err != nil {
		t.Fatal(err)
	}

	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if _, err := f2.Seek(logHeaderSize, 0); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := readRecord(f2)
	if err != nil {
		t.Fatalf("expected no hard error on torn read, got %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for torn record")
	}
}

// TestAppendLog_CorruptedCRC_Detected flips a byte inside a record's
// payload region and asserts the CRC check refuses it as a recovery-stop
// signal.
func TestAppendLog_CorruptedCRC_Detected(t *testing.T) {
	path, f := tmpLog(t)
	if _, err := f.Seek(logHeaderSize, 0); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0xAA}, 512)
	if _, err := writeRecord(f, 0, payload); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Flip a byte inside the payload (beyond header + frame).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[logHeaderSize+recordFrameOverhead+10] ^= 0xFF
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}

	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if _, err := f2.Seek(logHeaderSize, 0); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := readRecord(f2)
	if err != nil {
		t.Fatalf("no hard err, got %v", err)
	}
	if ok {
		t.Fatal("expected ok=false on CRC corruption")
	}
}

// TestAppendLog_HeaderCRC_Detected flips one bit inside bytes [0..28) of
// the header and asserts readLogHeader rejects it via ErrLogBadHeaderCRC
// (or ErrLogBadVersion if the flipped bit happened to land in the version
// field and changed it to a known-bad version first — both are valid
// rejections).
func TestAppendLog_HeaderCRC_Detected(t *testing.T) {
	_, f := tmpLog(t)
	path := f.Name()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a bit in the Version field (within [0..28)). The header CRC
	// covers this region, so the flip must be detected. We flip a high
	// bit to push Version outside the current logVersion (1) so either
	// ErrLogBadVersion or ErrLogBadHeaderCRC is a valid outcome.
	raw[5] ^= 0x01
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	_, err = readLogHeader(f2)
	if err == nil {
		t.Fatal("expected error on corrupted header")
	}
	if !errors.Is(err, ErrLogBadHeaderCRC) && !errors.Is(err, ErrLogBadVersion) {
		t.Fatalf("expected ErrLogBadHeaderCRC or ErrLogBadVersion, got %v", err)
	}
}

// TestAppendLog_AdvanceRollupOffset_Idempotent advances rollup_offset
// twice to the same value and asserts the header reads back cleanly both
// times — no CRC drift, no accumulated error.
func TestAppendLog_AdvanceRollupOffset_Idempotent(t *testing.T) {
	_, f := tmpLog(t)
	defer f.Close()
	if err := advanceRollupOffset(f, 1024); err != nil {
		t.Fatal(err)
	}
	if err := advanceRollupOffset(f, 1024); err != nil {
		t.Fatal(err)
	}
	h, err := readLogHeader(f)
	if err != nil {
		t.Fatal(err)
	}
	if h.RollupOffset != 1024 {
		t.Fatalf("rollup offset: got %d want 1024", h.RollupOffset)
	}
}

// TestAppendLog_AdvanceRollupOffset_Monotone_NotEnforcedByFunction
// documents that advanceRollupOffset itself does not enforce INV-03
// (monotone rollup_offset). The caller (CommitChunks in later plans) is
// responsible for the monotonicity check; this helper is a pure pwrite.
func TestAppendLog_AdvanceRollupOffset_Monotone_NotEnforcedByFunction(t *testing.T) {
	_, f := tmpLog(t)
	defer f.Close()
	if err := advanceRollupOffset(f, 1024); err != nil {
		t.Fatal(err)
	}
	// Moving backward succeeds at the function level — caller enforces.
	if err := advanceRollupOffset(f, 512); err != nil {
		t.Fatalf("advanceRollupOffset backwards unexpectedly failed: %v", err)
	}
	h, err := readLogHeader(f)
	if err != nil {
		t.Fatal(err)
	}
	if h.RollupOffset != 512 {
		t.Fatalf("rollup offset: got %d want 512", h.RollupOffset)
	}
}
