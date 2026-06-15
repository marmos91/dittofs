package fs

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// snapshotLogFile returns the current *logFile and per-file mutex for
// payloadID, or (nil, nil) if no fd is currently open for it.
func snapshotLogFile(bc *FSStore, payloadID string) *logFile {
	sh := bc.shardFor(payloadID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.logFDs[payloadID]
}

// TestAppendWrite_SyncError_EvictsFdAndKeepsLogIndexConsistent reproduces
// the desync bug where a groupCommit.Sync failure left lf.eofPos behind the
// fd position: the next AppendWrite would record a logIndex entry pointing
// at the orphaned, un-fsync'd frame instead of the new record, permanently
// wedging rollup.
//
// Before the fix, AppendWrite returned on Sync error WITHOUT evicting the
// fd, so the same *logFile (with a stale eofPos) was reused. After the fix,
// the fd is evicted exactly like the writeRecord-error path, so the next
// AppendWrite reopens fresh from the on-disk EOF and records a correct
// logIndex entry.
func TestAppendWrite_SyncError_EvictsFdAndKeepsLogIndexConsistent(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	payloadID := "file-syncerr"

	// First write succeeds and opens the fd.
	first := bytes.Repeat([]byte{0x11}, 256)
	if err := bc.AppendWrite(ctx, payloadID, first, 0); err != nil {
		t.Fatalf("first AppendWrite: %v", err)
	}

	lf := snapshotLogFile(bc, payloadID)
	if lf == nil {
		t.Fatal("no logFile after first write")
	}

	// Force the NEXT fsync to fail. The frame bytes still reach the OS fd
	// (writeRecord runs before Sync), advancing the fd position, but Sync
	// returns an error so eofPos is not advanced.
	wantErr := errors.New("injected fsync failure")
	lf.groupCommit = newGroupCommit(func() error { return wantErr })

	second := bytes.Repeat([]byte{0x22}, 256)
	err := bc.AppendWrite(ctx, payloadID, second, 4096)
	if err == nil {
		t.Fatal("AppendWrite under fsync failure: want error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("AppendWrite error: got %v, want wrapped %v", err, wantErr)
	}

	// The fd must have been evicted from the shard so the next call reopens
	// fresh. Reusing the stale *logFile (old eofPos) is exactly the wedge.
	if got := snapshotLogFile(bc, payloadID); got == lf {
		t.Fatal("fd not evicted after Sync error: stale logFile still in logFDs")
	}

	// A subsequent write must succeed, reopen fresh, and record a logIndex
	// entry whose logPos equals the reopened on-disk EOF — never the
	// pre-failure eofPos. We verify by reading the data back: the new
	// record's bytes must be returned for its offset.
	third := bytes.Repeat([]byte{0x33}, 256)
	if err := bc.AppendWrite(ctx, payloadID, third, 8192); err != nil {
		t.Fatalf("third AppendWrite after recovery: %v", err)
	}

	dest := make([]byte, 256)
	n, err := bc.ReadPayloadAt(ctx, payloadID, dest, 8192)
	if err != nil {
		t.Fatalf("ReadPayloadAt for recovered write: %v", err)
	}
	if n != 256 || !bytes.Equal(dest, third) {
		t.Fatalf("recovered write read-back mismatch: n=%d equal=%v", n, bytes.Equal(dest, third))
	}

	// The first, fully-committed write must still read back correctly.
	dest0 := make([]byte, 256)
	if _, err := bc.ReadPayloadAt(ctx, payloadID, dest0, 0); err != nil {
		t.Fatalf("ReadPayloadAt for first write: %v", err)
	}
	if !bytes.Equal(dest0, first) {
		t.Fatal("first committed write corrupted after Sync-error recovery")
	}
}
