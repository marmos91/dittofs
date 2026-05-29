package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
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

// newTestRuntime builds a Runtime backed by an in-memory SQLite cpstore,
// suitable for testing the new GetSnapshot / ListSnapshots / DeleteSnapshot
// wrappers without spinning up a metadata store or block store.
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })
	return New(cp)
}

// TestRuntimeSnapshot_NamePersists asserts a snapshot's Name column
// round-trips through the store layer (the column was previously absent so
// --name was silently dropped).
func TestRuntimeSnapshot_NamePersists(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const shareName = "alpha"
	snapID, err := rt.store.CreateSnapshot(ctx, &models.Snapshot{
		Name:           "weekly",
		ShareName:      shareName,
		State:          models.StateCreating,
		MetadataEngine: "memory",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	got, err := rt.GetSnapshot(ctx, shareName, snapID)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.Name != "weekly" {
		t.Fatalf("Name = %q, want weekly", got.Name)
	}
}

// TestRuntimeFailSnap_RecordsError asserts failSnap persists the cause's
// message onto the row so show/list surface the reason instead of
// "(no error message)", and that a failed->creating retry clears it.
func TestRuntimeFailSnap_RecordsError(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const shareName = "alpha"
	snapID, err := rt.store.CreateSnapshot(ctx, &models.Snapshot{
		ShareName:      shareName,
		State:          models.StateCreating,
		MetadataEngine: "memory",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	cause := errors.New("backup: disk full")
	rt.failSnap(shareName, snapID, cause)

	got, err := rt.GetSnapshot(ctx, shareName, snapID)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.State != models.StateFailed {
		t.Fatalf("State = %q, want failed", got.State)
	}
	if got.Error != "backup: disk full" {
		t.Fatalf("Error = %q, want %q", got.Error, "backup: disk full")
	}

	// A retry (failed -> creating) clears the stale error.
	if err := rt.store.UpdateSnapshotState(ctx, shareName, snapID, models.StateCreating); err != nil {
		t.Fatalf("UpdateSnapshotState->creating: %v", err)
	}
	got, err = rt.GetSnapshot(ctx, shareName, snapID)
	if err != nil {
		t.Fatalf("GetSnapshot after retry: %v", err)
	}
	if got.Error != "" {
		t.Fatalf("Error after retry = %q, want empty", got.Error)
	}
}

// TestRuntimeCreateSnapshot_MemoryLocalStore asserts a snapshot on a share
// whose local block store has no on-disk root fails up front with the typed
// ErrSnapshotLocalStoreUnsupported sentinel (mapped to 400) rather than an
// opaque 500.
func TestRuntimeCreateSnapshot_MemoryLocalStore(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const shareName = "alpha"
	// Inject a share with no local store dir (the in-memory backend case).
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: shareName})

	_, err := rt.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{})
	if !errors.Is(err, models.ErrSnapshotLocalStoreUnsupported) {
		t.Fatalf("CreateSnapshot err = %v, want errors.Is(ErrSnapshotLocalStoreUnsupported)", err)
	}
}

// TestRuntimeDeriveWaitCtx_IgnoresCallerCancel asserts the safety-snap wait
// ctx is rooted at runtimeCtx, so a cancelled caller (client disconnect)
// does not abort it, while runtime shutdown does. A caller deadline is
// preserved.
func TestRuntimeDeriveWaitCtx_IgnoresCallerCancel(t *testing.T) {
	rt := newTestRuntime(t)

	// Caller cancelled, no deadline → derived ctx stays alive.
	callerCtx, callerCancel := context.WithCancel(context.Background())
	callerCancel()
	waitCtx, waitCancel := rt.deriveWaitCtx(callerCtx)
	if waitCtx.Err() != nil {
		t.Fatalf("waitCtx already cancelled by caller cancel: %v", waitCtx.Err())
	}

	// Runtime shutdown DOES cancel it.
	rt.runtimeCancel()
	select {
	case <-waitCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("waitCtx not cancelled after runtime shutdown")
	}
	waitCancel()

	// Caller deadline is preserved on the derived ctx.
	rt2 := newTestRuntime(t)
	dl := time.Now().Add(123 * time.Hour)
	withDL, dlCancel := context.WithDeadline(context.Background(), dl)
	defer dlCancel()
	got, gotCancel := rt2.deriveWaitCtx(withDL)
	defer gotCancel()
	gotDL, ok := got.Deadline()
	if !ok || !gotDL.Equal(dl) {
		t.Fatalf("derived deadline = (%v, %v), want %v", gotDL, ok, dl)
	}
}

// TestRuntimeGetSnapshot_NotFound asserts ErrSnapshotNotFound propagates
// from the store through the Runtime wrapper.
func TestRuntimeGetSnapshot_NotFound(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := rt.GetSnapshot(ctx, "missing-share", "missing-id")
	if !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("GetSnapshot err = %v, want errors.Is(ErrSnapshotNotFound)", err)
	}
}

// TestRuntimeListSnapshots_Empty asserts the wrapper returns a non-nil
// empty slice (so JSON encodes [] not null) when the share has no rows.
func TestRuntimeListSnapshots_Empty(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := rt.ListSnapshots(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if got == nil {
		t.Fatal("ListSnapshots: got nil slice, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("ListSnapshots: len = %d, want 0", len(got))
	}
}

// TestRuntimeDeleteSnapshot_HappyPath asserts the wrapper deletes the
// store row and the on-disk dir under the per-share delete lock.
func TestRuntimeDeleteSnapshot_HappyPath(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const shareName = "alpha"

	// Inject the share into the registry and assign a tempdir as the
	// per-share local store dir so DeleteSnapshot can compute and wipe
	// the snapshot directory.
	localStoreDir := t.TempDir()
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: shareName})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	// Seed the store row.
	snap := &models.Snapshot{
		ShareName:      shareName,
		State:          models.StateReady,
		MetadataEngine: "memory",
	}
	snapID, err := rt.store.CreateSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Create the on-disk snapshot dir + a marker file under it.
	snapDir := (&models.Snapshot{ID: snapID}).SnapshotDir(localStoreDir)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		t.Fatalf("MkdirAll %q: %v", snapDir, err)
	}
	markerPath := filepath.Join(snapDir, "marker")
	if err := os.WriteFile(markerPath, []byte("alive"), 0o600); err != nil {
		t.Fatalf("WriteFile %q: %v", markerPath, err)
	}

	if err := rt.DeleteSnapshot(ctx, shareName, snapID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	// Row is gone.
	_, gerr := rt.store.GetSnapshot(ctx, shareName, snapID)
	if !errors.Is(gerr, models.ErrSnapshotNotFound) {
		t.Fatalf("post-delete GetSnapshot err = %v, want ErrSnapshotNotFound", gerr)
	}

	// Marker file + dir gone.
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker file still present after DeleteSnapshot: stat err = %v", err)
	}
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir still present after DeleteSnapshot: stat err = %v", err)
	}

	// Lock was released: a follow-up Lock() must succeed without blocking.
	lock := rt.snapshotDeleteLock(shareName)
	gotLock := make(chan struct{})
	go func() {
		lock.Lock()
		close(gotLock)
		lock.Unlock()
	}()
	select {
	case <-gotLock:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot delete lock not released after DeleteSnapshot")
	}
}

// TestRuntimeDeleteSnapshot_RefusesInFlight asserts DeleteSnapshot refuses
// with ErrSnapshotInFlight when an orchestration goroutine is registered for
// the same snapID, so a delete cannot race a running create/retry.
func TestRuntimeDeleteSnapshot_RefusesInFlight(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const shareName = "alpha"
	localStoreDir := t.TempDir()
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: shareName})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	snap := &models.Snapshot{ShareName: shareName, State: models.StateCreating, MetadataEngine: "memory"}
	snapID, err := rt.store.CreateSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Plant an in-flight registry entry (mimics a running orchestration).
	_, _, entry := rt.registerSnapInFlight(shareName, snapID)

	if derr := rt.DeleteSnapshot(ctx, shareName, snapID); !errors.Is(derr, models.ErrSnapshotInFlight) {
		t.Fatalf("DeleteSnapshot err = %v, want errors.Is(ErrSnapshotInFlight)", derr)
	}

	// Row must still exist (delete refused).
	if _, gerr := rt.store.GetSnapshot(ctx, shareName, snapID); gerr != nil {
		t.Fatalf("row deleted despite in-flight refusal: %v", gerr)
	}

	// Drain the planted entry, then delete succeeds.
	rt.unregisterSnap(shareName, snapID, entry)
	entry.wg.Done()
	if derr := rt.DeleteSnapshot(ctx, shareName, snapID); derr != nil {
		t.Fatalf("DeleteSnapshot after drain: %v", derr)
	}
}

// TestRuntimeDeleteSnapshot_NotFound asserts ErrSnapshotNotFound from the
// store propagates verbatim and the on-disk wipe step is NOT attempted.
func TestRuntimeDeleteSnapshot_NotFound(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rt.DeleteSnapshot(ctx, "alpha", "no-such-snap")
	if !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("DeleteSnapshot err = %v, want errors.Is(ErrSnapshotNotFound)", err)
	}
}

// TestRuntimeDeleteSnapshot_RejectsPathTraversal asserts a snapID with
// path-separator characters is rejected as ErrSnapshotNotFound before any
// store or filesystem touch.
func TestRuntimeDeleteSnapshot_RejectsPathTraversal(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, bad := range []string{"../escape", "a/b", `a\b`} {
		err := rt.DeleteSnapshot(ctx, "alpha", bad)
		if !errors.Is(err, models.ErrSnapshotNotFound) {
			t.Fatalf("DeleteSnapshot(%q) err = %v, want ErrSnapshotNotFound", bad, err)
		}
	}
}
