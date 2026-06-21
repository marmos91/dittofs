package fs

import (
	"bytes"
	"context"
	"testing"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newC3BudgetStore builds an FSStore wired with a real rollup coordinator so
// rollup passes complete end-to-end, exercising the logBytesTotal accounting.
// It is an in-package (package fs) helper so the C3 tests can read the
// unexported logBytesTotal counter directly without the *ForTest seams.
func newC3BudgetStore(t *testing.T, maxLogBytes int64) *FSStore {
	t.Helper()
	mem := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(t.TempDir(), 1<<30, mem, FSStoreOptions{
		MaxLogBytes:     maxLogBytes,
		RollupWorkers:   1,
		StabilizationMS: 1,
		RollupStore:     mem,
		SyncedHashStore: mem,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// TestC3_OverwriteSameRegion_BudgetDrains pins the C3 budget-leak fix: writing
// and rolling up the SAME file region repeatedly must keep logBytesTotal an
// exact running total of un-rolled-up record bytes — it must NOT ratchet
// upward. Before the fix the budget was released by the compaction-fence
// position delta, which under repeated same-offset overwrites under-counted
// the bytes actually retired, leaking ~one record's worth of budget per pass
// until the global pressure ceiling pinned and every writer timed out.
func TestC3_OverwriteSameRegion_BudgetDrains(t *testing.T) {
	bc := newC3BudgetStore(t, 1<<30)
	ctx := context.Background()
	const pid = "c3-overwrite"
	const sz = 64 * 1024
	payload := bytes.Repeat([]byte{0xAB}, sz)

	for pass := 0; pass < 30; pass++ {
		if err := bc.AppendWrite(ctx, pid, payload, 0); err != nil {
			t.Fatalf("pass %d AppendWrite: %v", pass, err)
		}
		// ForceRollup bypasses the stabilization window so the just-written
		// record is consumed deterministically this pass.
		if err := bc.rollupFile(ctx, pid, true); err != nil {
			t.Fatalf("pass %d rollup: %v", pass, err)
		}
	}

	// After 30 force-rolled-up overwrites of one region the budget must be
	// near zero — at most a single straggling record. A leak shows up as
	// ~30x the record size.
	const frame = recordFrameOverhead + sz
	if got := bc.logBytesTotal.Load(); got > int64(frame) {
		t.Fatalf("budget leaked across overwrite passes: logBytesTotal=%d, want <= one record (%d)", got, frame)
	}
}

// TestC3_DeleteMidWrite_ReclaimsBudget pins the dominant storm leak: deleting a
// payload that still has un-rolled-up records must release their reserved
// pressure budget. Before the fix DeleteAppendLog dropped the logIndex without
// reclaiming logBytesTotal, so create/delete churn ratcheted the global budget
// to maxLogBytes and wedged every writer on ErrPressureTimeout.
func TestC3_DeleteMidWrite_ReclaimsBudget(t *testing.T) {
	bc := newC3BudgetStore(t, 1<<30)
	ctx := context.Background()
	const sz = 128 * 1024
	payload := bytes.Repeat([]byte{0xCD}, sz)

	const files = 50
	for i := 0; i < files; i++ {
		pid := pidN(i)
		if err := bc.AppendWrite(ctx, pid, payload, 0); err != nil {
			t.Fatalf("file %d AppendWrite: %v", i, err)
		}
	}
	// Each write reserved frame bytes; nothing has rolled up yet.
	const frame = recordFrameOverhead + sz
	if got, want := bc.logBytesTotal.Load(), int64(files*frame); got != want {
		t.Fatalf("pre-delete budget = %d, want %d", got, want)
	}

	// Delete every payload mid-write (no rollup ran). All reserved budget
	// must be reclaimed.
	for i := 0; i < files; i++ {
		if err := bc.DeleteAppendLog(ctx, pidN(i)); err != nil {
			t.Fatalf("file %d DeleteAppendLog: %v", i, err)
		}
	}
	if got := bc.logBytesTotal.Load(); got != 0 {
		t.Fatalf("budget leaked after deleting un-rolled-up payloads: logBytesTotal=%d, want 0", got)
	}
}

func pidN(i int) string {
	return "c3-del/" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}
