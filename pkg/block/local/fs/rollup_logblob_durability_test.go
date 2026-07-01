package fs

import (
	"bytes"
	"context"
	"testing"
	"time"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLogBlobStoreWithRollup creates an FSStore with both a LocalChunkIndex
// (enabling the log-blob write path) and a RollupStore (enabling rollup
// passes). Used by logblob durability tests only.
func newLogBlobStoreWithRollup(t *testing.T) *FSStore {
	t.Helper()
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	idx := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(t.TempDir(), 0, nil, FSStoreOptions{
		LocalChunkIndex: idx,
		RollupStore:     rs,
		MaxLogBytes:     1 << 30,
		StabilizationMS: 1,
		RollupWorkers:   1,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// TestRollup_LogBlob_SyncBeforeFenceAdvance is the C1 durability guard for
// the log-blob rollup path. It asserts that logBlob.Sync() is called at least
// once per rollup pass — BEFORE SetRollupOffset advances the fence — so that
// rolled-up chunk bytes are durable on disk before recovery can be instructed
// to seek past them.
//
// The test FAILS against code that has no Sync call (counter stays at 0) and
// PASSES after the fix inserts logBlob.Sync() in Phase C before SetRollupOffset.
func TestRollup_LogBlob_SyncBeforeFenceAdvance(t *testing.T) {
	bc := newLogBlobStoreWithRollup(t)
	ctx := context.Background()
	const pid = "logblob/durability/sync"

	// Write enough data to give the rollup pass something to do.
	if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{0xAB}, 4096), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Wait for the interval to age past the stabilization window.
	deadline := time.Now().Add(2 * time.Second)
	for !bc.EarliestStableForTest(pid) {
		if time.Now().After(deadline) {
			t.Fatal("interval never stabilized")
		}
		time.Sleep(2 * time.Millisecond)
	}

	syncBefore := bc.LogBlobRollupSyncCountForTest()

	if err := bc.ForceRollupForTest(ctx, pid); err != nil {
		t.Fatalf("ForceRollupForTest: %v", err)
	}

	syncAfter := bc.LogBlobRollupSyncCountForTest()
	if syncAfter <= syncBefore {
		t.Fatalf("logBlob.Sync not called during rollup pass: count before=%d after=%d; "+
			"want after > before (durability: blob bytes must be fsynced before fence advances)",
			syncBefore, syncAfter)
	}
}

// TestRollup_CASOnly_NilLogBlobGuard asserts that the nil logBlob guard is
// intact: a CAS-only FSStore (no LocalChunkIndex, no logBlob) must complete a
// rollup pass without panicking and must not increment the logBlob sync counter.
func TestRollup_CASOnly_NilLogBlobGuard(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   1,
		StabilizationMS: 1,
		RollupStore:     rs,
	})
	ctx := context.Background()
	const pid = "logblob/cas-only/nil-guard"

	if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{0xBC}, 4096), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !bc.EarliestStableForTest(pid) {
		if time.Now().After(deadline) {
			t.Fatal("interval never stabilized")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := bc.ForceRollupForTest(ctx, pid); err != nil {
		t.Fatalf("ForceRollupForTest: %v", err)
	}

	if n := bc.LogBlobRollupSyncCountForTest(); n != 0 {
		t.Fatalf("CAS-only store must not increment logBlob sync counter; got %d", n)
	}
}
