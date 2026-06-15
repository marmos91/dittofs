package fs

import (
	"bytes"
	"context"
	"testing"
)

// TestReadPayloadAt_IndexAssisted_OutOfOrderOverlap verifies the
// index-assisted read path returns the correct bytes for out-of-order
// arrivals with overlapping offsets — applying records in logPos (arrival)
// order so the later write at an overlapping offset wins, matching the
// full-scan semantics.
func TestReadPayloadAt_IndexAssisted_OutOfOrderOverlap(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	id := "file-idx-overlap"

	// Arrive out of file-offset order, with an overlap at [1024,1280): the
	// later write (0x22) must win.
	a := bytes.Repeat([]byte{0xAA}, 256)
	b := bytes.Repeat([]byte{0xBB}, 256)
	overlapOld := bytes.Repeat([]byte{0x11}, 256)
	overlapNew := bytes.Repeat([]byte{0x22}, 256)

	if err := bc.AppendWrite(ctx, id, b, 8192); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if err := bc.AppendWrite(ctx, id, overlapOld, 1024); err != nil {
		t.Fatalf("write overlapOld: %v", err)
	}
	if err := bc.AppendWrite(ctx, id, a, 0); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := bc.AppendWrite(ctx, id, overlapNew, 1024); err != nil {
		t.Fatalf("write overlapNew: %v", err)
	}

	// Read each region back and check.
	check := func(off uint64, want []byte) {
		t.Helper()
		dest := make([]byte, len(want))
		n, err := bc.ReadPayloadAt(ctx, id, dest, off)
		if err != nil {
			t.Fatalf("ReadPayloadAt(off=%d): %v", off, err)
		}
		if n != len(want) || !bytes.Equal(dest, want) {
			t.Fatalf("ReadPayloadAt(off=%d): mismatch (n=%d)", off, n)
		}
	}
	check(0, a)
	check(8192, b)
	check(1024, overlapNew) // last write wins

	// A read that touches only the 'a' region must not pay to scan the
	// 8192 record — correctness is what we can assert here: the window is
	// satisfied entirely from the index-selected record.
	dest := make([]byte, 128)
	if _, err := bc.ReadPayloadAt(ctx, id, dest, 64); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	if !bytes.Equal(dest, a[64:64+128]) {
		t.Fatal("partial read mismatch")
	}
}

// TestReplayViaScan_Fallback verifies the index-free fallback still works
// when no logIndex is wired (replayViaScan path), preserving last-write-wins
// over a full sequential scan.
func TestReplayViaScan_Fallback(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	id := "file-scan-fallback"

	if err := bc.AppendWrite(ctx, id, bytes.Repeat([]byte{0x11}, 256), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := bc.AppendWrite(ctx, id, bytes.Repeat([]byte{0x22}, 256), 0); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Drop the logIndex to force the fallback scan path. The fd and lock
	// stay in place so replayLogIntoDest still finds the log.
	sh := bc.shardFor(id)
	sh.mu.Lock()
	delete(sh.logIndices, id)
	sh.mu.Unlock()

	dest := make([]byte, 256)
	if _, err := bc.ReadPayloadAt(ctx, id, dest, 0); err != nil {
		t.Fatalf("ReadPayloadAt: %v", err)
	}
	if !bytes.Equal(dest, bytes.Repeat([]byte{0x22}, 256)) {
		t.Fatal("fallback scan did not honour last-write-wins")
	}
}
