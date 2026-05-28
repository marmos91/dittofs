package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
)

// TestWaitForSnapshot_FallbackWhenAlreadyComplete asserts the
// "no in-flight registry entry / chan already closed" path: WaitForSnapshot
// falls back directly to GetSnapshot and returns the row + nil orchestration
// error so callers see the final state column.
//
// Covers D-23-19: WaitForSnapshot is poll-free when in-flight (consumes the
// per-snap result chan) but degrades to a direct GetSnapshot read when the
// snapshot's orchestration has already completed and the registry entry was
// reaped.
func TestWaitForSnapshot_FallbackWhenAlreadyComplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	const shareName = "alpha"

	// Insert a snapshot row directly (no registry entry) — mimics the
	// "orchestration goroutine already exited and unregistered itself"
	// state.
	snapID, err := rt.store.CreateSnapshot(ctx, &models.Snapshot{
		ShareName:      shareName,
		State:          models.StateCreating,
		MetadataEngine: "memory",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if err := rt.store.UpdateSnapshotState(ctx, shareName, snapID, models.StateReady); err != nil {
		t.Fatalf("UpdateSnapshotState->ready: %v", err)
	}

	got, err := rt.WaitForSnapshot(ctx, shareName, snapID)
	if err != nil {
		t.Fatalf("WaitForSnapshot: err = %v, want nil (no in-flight entry → direct GetSnapshot)", err)
	}
	if got == nil {
		t.Fatalf("WaitForSnapshot: got nil snapshot, want non-nil")
	}
	if got.ID != snapID {
		t.Fatalf("WaitForSnapshot: id = %q, want %q", got.ID, snapID)
	}
	if got.State != models.StateReady {
		t.Fatalf("WaitForSnapshot: state = %q, want %q", got.State, models.StateReady)
	}
}

// TestWaitForSnapshot_NotFound asserts ErrSnapshotNotFound propagates
// from the GetSnapshot fallback when there's no row at all and no registry
// entry. errors.Is must match the sentinel through the wrap.
func TestWaitForSnapshot_NotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	_, err = rt.WaitForSnapshot(ctx, "alpha", "no-such-id")
	if err == nil {
		t.Fatal("WaitForSnapshot: err = nil, want ErrSnapshotNotFound")
	}
	if !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("WaitForSnapshot: err = %v, want errors.Is(ErrSnapshotNotFound)", err)
	}
}

// TestWaitForSnapshot_CtxCancelDuringWait asserts ctx cancellation
// while blocked on the per-snap result chan returns ctx.Err() and does
// not consult GetSnapshot afterward.
func TestWaitForSnapshot_CtxCancelDuringWait(t *testing.T) {
	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	const shareName = "alpha"

	// Insert the row, then plant an in-flight registry entry pointing at
	// a chan no one will ever signal. WaitForSnapshot must select on
	// ctx.Done().
	bg := context.Background()
	snapID, err := rt.store.CreateSnapshot(bg, &models.Snapshot{
		ShareName:      shareName,
		State:          models.StateCreating,
		MetadataEngine: "memory",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Plant a registry entry by reusing registerSnapInFlight. The child
	// ctx + cancel are discarded; we only care that the done chan exists.
	_, _, _ = rt.registerSnapInFlight(shareName, snapID)

	ctx, cancel := context.WithCancel(bg)
	cancel() // pre-cancel — guarantees Wait returns ctx.Err() without racing.

	got, err := rt.WaitForSnapshot(ctx, shareName, snapID)
	if err == nil {
		t.Fatal("WaitForSnapshot: err = nil, want ctx.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForSnapshot: err = %v, want errors.Is(context.Canceled)", err)
	}
	if got != nil {
		t.Fatalf("WaitForSnapshot: got = %v, want nil on ctx cancel", got)
	}

	// Cleanup the planted registry entry so other goroutines/test teardown
	// don't trip over an unowned WG.
	rt.snapInFlightMu.Lock()
	entry := rt.snapInFlight[shareName]
	delete(rt.snapInFlight, shareName)
	rt.snapInFlightMu.Unlock()
	if entry != nil {
		// Decrement the WG slot so it does not leak. registerSnapInFlight
		// did wg.Add(1); no goroutine will ever wg.Done it.
		entry.wg.Done()
	}
}
