package runtime

import (
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestRestore_ConcurrentRejectedWhileLocked is the deterministic guard test
// for the per-share restore lock: while a restore holds the lock, any second
// RestoreSnapshot on the same share fails fast with ErrRestoreInProgress
// rather than interleaving the destructive metadata Reset + dump replay.
//
// We hold the lock directly (modeling an in-flight restore) so the assertion
// is timing-independent, then release and confirm a subsequent restore
// proceeds normally.
func TestRestore_ConcurrentRejectedWhileLocked(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"a.bin", "b.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	fx.deleteFileCascade(ctx, files[0])

	// Simulate an in-flight restore by holding the same per-share lock the
	// restore path would take.
	lock := fx.rt.restoreLock(fx.shareName)
	lock.Lock()

	_, err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrRestoreInProgress) {
		lock.Unlock()
		t.Fatalf("RestoreSnapshot while locked: err = %v, want ErrRestoreInProgress", err)
	}

	// Release; a subsequent restore must succeed and bring files[0] back.
	lock.Unlock()
	if _, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{}); err != nil {
		t.Fatalf("RestoreSnapshot after unlock: %v", err)
	}
	if _, gerr := fx.meta.GetFile(ctx, files[0].handle); gerr != nil {
		t.Fatalf("files[0] not restored after unlock: %v", gerr)
	}
}

// TestRestore_ConcurrentFanout launches several RestoreSnapshot calls on the
// same share at once. The per-share lock serializes them: a call either wins
// the lock and runs to completion, or fails fast with ErrRestoreInProgress —
// never a third error class, and never two restores interleaving their
// destructive Reset+replay. Because the lock is released between runs and each
// restore is idempotent on the snapshot, MORE THAN ONE call may succeed (in
// sequence) when an earlier one finishes before a later goroutine reaches the
// TryLock; that is expected and safe. The invariants asserted are therefore:
// at least one success, every outcome is success-or-ErrRestoreInProgress, and
// the final state is the consistent restored state. Run with -race to catch
// data races in the restore path under contention.
func TestRestore_ConcurrentFanout(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"x.bin", "y.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	// Mutate: delete files[0]; every successful restore must bring it back.
	fx.deleteFileCascade(ctx, files[0])

	const goroutines = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded, rejected int
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all at once to maximize overlap
			_, rerr := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case rerr == nil:
				succeeded++
			case errors.Is(rerr, models.ErrRestoreInProgress):
				rejected++
			default:
				t.Errorf("unexpected restore error class: %v", rerr)
			}
		}()
	}
	close(start)
	wg.Wait()

	if succeeded < 1 {
		t.Fatalf("no restore succeeded (succeeded=%d rejected=%d)", succeeded, rejected)
	}
	if succeeded+rejected != goroutines {
		t.Fatalf("accounting mismatch: succeeded=%d rejected=%d total!=%d", succeeded, rejected, goroutines)
	}
	t.Logf("concurrent restores: %d succeeded, %d rejected with ErrRestoreInProgress", succeeded, rejected)

	// Final state must be the restored state regardless of how the race
	// resolved: files[0] (deleted post-snapshot) is back, files[1] present.
	if _, gerr := fx.meta.GetFile(ctx, files[0].handle); gerr != nil {
		t.Fatalf("files[0] not present in final restored state: %v", gerr)
	}
	if _, gerr := fx.meta.GetFile(ctx, files[1].handle); gerr != nil {
		t.Fatalf("files[1] missing in final restored state: %v", gerr)
	}
}
