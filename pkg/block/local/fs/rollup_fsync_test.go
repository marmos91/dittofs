package fs

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAdvanceRollupOffset_ValidHeader is the correctness invariant the
// single-fsync collapse (#1411 Lever 2) must preserve: after advancing, the
// on-disk header carries the new rollup_offset AND a matching CRC, so a
// subsequent reader accepts it.
func TestAdvanceRollupOffset_ValidHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	f, err := initLogFile(path, 12345)
	if err != nil {
		t.Fatalf("initLogFile: %v", err)
	}
	defer func() { _ = f.Close() }()

	const newOffset = uint64(4096)
	if err := advanceRollupOffset(f, newOffset); err != nil {
		t.Fatalf("advanceRollupOffset: %v", err)
	}

	// Re-read through the same validation path a fresh boot uses.
	hdr, err := readLogHeader(f)
	if err != nil {
		t.Fatalf("readLogHeader after advance: %v (header must stay valid)", err)
	}
	if hdr.RollupOffset != newOffset {
		t.Fatalf("RollupOffset = %d, want %d", hdr.RollupOffset, newOffset)
	}

	// And from a cold reopen (durability, not just page cache).
	f2, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = f2.Close() }()
	hdr2, err := readLogHeader(f2)
	if err != nil {
		t.Fatalf("readLogHeader after reopen: %v", err)
	}
	if hdr2.RollupOffset != newOffset {
		t.Fatalf("reopened RollupOffset = %d, want %d", hdr2.RollupOffset, newOffset)
	}
}

// TestAdvanceRollupOffset_TornWriteDetected pins the crash-safety property the
// single-fsync collapse relies on: any header where the rollup_offset and the
// CRC disagree (the exact state a torn/partial write would leave — new offset
// bytes reached the platter but the CRC did not, or vice versa) is rejected by
// the boot-time validation as ErrLogBadHeaderCRC, which routes to the existing
// safe re-init path. The offset field [8:16) and the CRC field [28:32) both
// live within the first 512-byte sector, so a single fsync persists them as a
// unit (both-new or both-old); this test simulates the only observable failure
// mode — a mismatch — and asserts it is caught.
func TestAdvanceRollupOffset_TornWriteDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	f, err := initLogFile(path, 12345)
	if err != nil {
		t.Fatalf("initLogFile: %v", err)
	}
	defer func() { _ = f.Close() }()

	// Simulate a torn write: poke a new rollup_offset into [8:16) but leave
	// the CRC at [28:32) stale (as if the offset write reached disk but the
	// CRC write did not).
	var off [8]byte
	binary.LittleEndian.PutUint64(off[:], 999999)
	if _, err := f.WriteAt(off[:], 8); err != nil {
		t.Fatalf("poke offset: %v", err)
	}

	_, err = readLogHeader(f)
	if !errors.Is(err, ErrLogBadHeaderCRC) {
		t.Fatalf("torn header read err = %v, want ErrLogBadHeaderCRC (must be detected as corrupt)", err)
	}
}
