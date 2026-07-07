package fs

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestUnstableWriteThenCommit_SurvivesReopen is the PR3 durability gate: an
// UNSTABLE (deferred-fsync) AppendWrite followed by SyncPayload (the COMMIT
// barrier) makes the record durable, so after Close + reopen + recovery the
// bytes read back. This is the "UNSTABLE writes + COMMIT → recover → data
// present" half of the crash contract.
func TestUnstableWriteThenCommit_SurvivesReopen(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30, StabilizationMS: 100000})
	ctx := context.Background()
	const payloadID = "commit-survives"
	payload := bytes.Repeat([]byte{0x5A}, 4096)

	// UNSTABLE write: no inline fsync.
	if err := bc.AppendWrite(ctx, payloadID, payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	if got := bc.LogFsyncCountForTest(); got != 0 {
		t.Fatalf("UNSTABLE write fsynced %d times, want 0 (deferred)", got)
	}

	// COMMIT barrier makes it durable and advances the watermark.
	if err := bc.SyncPayload(ctx, payloadID); err != nil {
		t.Fatalf("SyncPayload: %v", err)
	}
	if got := bc.LogFsyncCountForTest(); got < 1 {
		t.Fatalf("SyncPayload issued %d fsyncs, want >= 1", got)
	}
	if pos, ok := bc.SyncedPosForTest(payloadID); !ok || pos == 0 {
		t.Fatalf("syncedPos not advanced after COMMIT: pos=%d ok=%v", pos, ok)
	}

	baseDir := bc.BaseDirForTest()
	rs := bc.RollupStoreForTest()
	if err := bc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen (fresh store + Recover) simulates a restart; the committed
	// record must replay from the append log.
	re, err := ReopenForTest(baseDir, rs)
	if err != nil {
		t.Fatalf("ReopenForTest: %v", err)
	}
	t.Cleanup(func() { _ = re.Close() })

	dest := make([]byte, len(payload))
	n, err := re.ReadPayloadAt(ctx, payloadID, dest, 0)
	if err != nil {
		t.Fatalf("ReadPayloadAt after reopen: %v", err)
	}
	if n != len(payload) || !bytes.Equal(dest, payload) {
		t.Fatalf("committed data lost across reopen: n=%d equal=%v", n, bytes.Equal(dest, payload))
	}
}

// TestSyncPayload_FsyncError_Surfaced proves a durability point that cannot
// fsync does NOT silently ack: SyncPayload propagates the fsync failure so the
// COMMIT/CLOSE caller reports an error and the client re-drives. Mirrors the
// rollupPreSyncFailHook fault-injection style.
func TestSyncPayload_FsyncError_Surfaced(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	const payloadID = "commit-fsync-fails"

	if err := bc.AppendWrite(ctx, payloadID, []byte("data"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	// Watermark baseline (seeded at the on-disk header EOF; the UNSTABLE write
	// advanced eofPos but not syncedPos).
	base, _ := bc.SyncedPosForTest(payloadID)

	wantErr := errors.New("commit fsync exploded")
	appendSyncFailHook = func() error { return wantErr }
	defer func() { appendSyncFailHook = nil }()

	err := bc.SyncPayload(ctx, payloadID)
	if err == nil {
		t.Fatal("SyncPayload returned nil under fsync failure; want error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("SyncPayload error: got %v, want wrapped %v", err, wantErr)
	}
	// Watermark must NOT advance on a failed fsync (a later retry must fsync).
	if pos, ok := bc.SyncedPosForTest(payloadID); ok && pos != base {
		t.Fatalf("syncedPos advanced despite fsync failure: pos=%d, want %d", pos, base)
	}
}

// TestSyncPayload_NoOpForUnknownPayload asserts SyncPayload is a safe no-op
// when the payload has no open log (never written, or already rolled up into
// durable CAS and its fd retired) — there is nothing local to fsync.
func TestSyncPayload_NoOpForUnknownPayload(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	if err := bc.SyncPayload(context.Background(), "never-written"); err != nil {
		t.Fatalf("SyncPayload on unknown payload: got %v, want nil", err)
	}
	if got := bc.LogFsyncCountForTest(); got != 0 {
		t.Fatalf("no-op SyncPayload fsynced %d times, want 0", got)
	}
}
