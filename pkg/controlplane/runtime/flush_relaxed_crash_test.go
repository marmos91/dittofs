package runtime

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/common"
)

// TestFlushRelaxed_CrashBeforeMetadataFsync_SizeReconciled is THE merge gate for
// #1687 (flush->relaxed). It proves the reconciliation safety net (piece 2) makes
// the relaxed metadata-size commit (piece 1) crash-safe: ACK'd bytes are never
// truncated even if the deferred metadata fsync never ran.
//
// Modelled scenario: a client WRITE was ACK'd via the relaxed path — the byte
// data was fsync'd into the local journal (WriteToBlockStore + CommitBlockStore),
// but the deferred metadata size commit had NOT yet reached stable storage when
// the process crashed. So metadata.Size is still the pre-write value (0) while the
// journal holds N durable bytes. We deliberately do NOT call
// FlushPendingWriteForFile and do NOT roll up (data stays in the synced append-log
// segments that recover() rebuilds), so the metadata store is untouched.
//
// After simulateRestart (real close+reopen of the metadata store, and a fresh
// AddShare that runs reconcileMetadataSizeFromJournal before the share is visible)
// the file's metadata.Size must equal N and the N bytes must read back intact.
//
// Gate property: without piece 2 (reconciliation), metadata.Size stays 0 after the
// restart and the Size assertion below fails — i.e. this test genuinely fails on
// piece-1-without-piece-2, which is what makes it a real safety proof rather than a
// hollow green.
func TestFlushRelaxed_CrashBeforeMetadataFsync_SizeReconciled(t *testing.T) {
	const n = 256 * 1024 // multi-interval, small enough for the postgres row
	for _, bk := range byteVerifyBackends(t) {
		bk := bk
		t.Run(bk.name, func(t *testing.T) {
			if bk.skip != "" {
				t.Skip(bk.skip)
			}
			if bk.reopen == nil {
				t.Skipf("%s cannot survive a restart (no durable reopen)", bk.name)
			}

			meta, metaType := bk.open(t)
			fx := newByteVerifyFixture(t, meta, metaType)
			defer fx.close()

			ctx := context.Background()
			data := distinctBytes(n, 0x5A)

			// (1) Create the file (metadata Size == 0, durable).
			pid := fx.createEmptyFile(ctx, "acked.bin")

			// (2) Make the bytes journal-durable WITHOUT touching metadata:
			// WriteToBlockStore -> CommitBlockStore fsyncs the append-log segment.
			// No DrainRollups and no FlushPendingWriteForFile: this is exactly the
			// state after a relaxed WRITE ack whose metadata fsync never ran.
			if err := common.WriteToBlockStore(ctx, fx.bs, pid, data, 0); err != nil {
				t.Fatalf("WriteToBlockStore: %v", err)
			}
			if err := common.CommitBlockStore(ctx, fx.bs, pid); err != nil {
				t.Fatalf("CommitBlockStore: %v", err)
			}

			// (3) Precondition: the crash-window state we are modelling.
			// metadata.Size is stale (0) while the journal holds N durable bytes.
			if got := fx.getFile(ctx, "acked.bin").Size; got != 0 {
				t.Fatalf("precondition: metadata.Size = %d, want 0 (test must model a stale size)", got)
			}
			if js, ok := fx.bs.Local().FileSize(ctx, string(pid)); !ok || js != int64(n) {
				t.Fatalf("precondition: journal FileSize = (%d, %v), want (%d, true) — data not journal-durable", js, ok, n)
			}

			// (4) Real restart: close+reopen metadata, fresh AddShare runs the
			// reconciliation hook before the share is visible to any handler.
			fx.simulateRestart(bk.reopen)

			// (5) The gate: metadata.Size was grown to the journal high-water mark.
			if got := fx.getFile(ctx, "acked.bin").Size; got != uint64(n) {
				t.Fatalf("ACK'd data truncated: metadata.Size = %d after restart, want %d "+
					"(reconcileMetadataSizeFromJournal did not run / did not grow the size)", got, n)
			}

			// (6) And the ACK'd bytes read back byte-identical.
			if got := fx.readFile(ctx, pid, n); !bytes.Equal(got, data) {
				t.Fatalf("ACK'd bytes not intact after restart: %s", firstDiff(data, got))
			}
		})
	}
}
