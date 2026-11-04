package runtime

import (
	"context"
	"errors"
	"github.com/marmos91/dittofs/pkg/block"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
)

// snapTestTimeout bounds a wedged snapshot operation so a stuck test fails
// instead of running to the suite's own limit. It is not a latency assertion:
// every call made under it completes in well under a second, and nothing here
// asserts that the deadline fires. A bound close to the normal duration makes
// a machine that stalls — under a loaded scheduler, a long collection pause,
// memory pressure — indistinguishable from a genuine wedge, and reports it as
// a failure of whichever operation happened to be in flight.
const snapTestTimeout = 30 * time.Second

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
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
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
	setJournalRoot(t, rt)

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
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
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
	setJournalRoot(t, rt)

	_, err = rt.WaitForSnapshot(ctx, "alpha", "no-such-id")
	if err == nil {
		t.Fatal("WaitForSnapshot: err = nil, want ErrSnapshotNotFound")
	}
	if !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("WaitForSnapshot: err = %v, want errors.Is(ErrSnapshotNotFound)", err)
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
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
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

// TestRuntimeCreateSnapshot_NoContentStore asserts a snapshot, which captures
// a share's content, answers block.ErrNoContentStore while none is wired.
func TestRuntimeCreateSnapshot_NoContentStore(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
	defer cancel()

	const shareName = "alpha"
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: shareName})

	_, err := rt.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{})
	if !errors.Is(err, block.ErrNoContentStore) {
		t.Fatalf("CreateSnapshot err = %v, want errors.Is(block.ErrNoContentStore)", err)
	}
}

// TestRuntimeGetSnapshot_NotFound asserts ErrSnapshotNotFound propagates
// from the store through the Runtime wrapper.
func TestRuntimeGetSnapshot_NotFound(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
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
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
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
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
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

// TestRuntimeDeleteSnapshot_RefusesMarkerProtected asserts DeleteSnapshot
// refuses to delete a snapshot that an in-progress restore depends on as its
// rollback target — the safety snapshot (and the target snapshot) named by the
// per-share restore marker. Deleting the safety snap mid-restore would destroy
// the sole rollback primitive (#8 H2). Once the marker is cleared (restore
// done), the delete succeeds.
func TestRuntimeDeleteSnapshot_RefusesMarkerProtected(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
	defer cancel()

	const shareName = "alpha"
	localStoreDir := t.TempDir()
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: shareName})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	// Seed two ready snapshots: the safety snap and the target snap.
	safetySnap := &models.Snapshot{ShareName: shareName, State: models.StateReady, MetadataEngine: "memory"}
	safetyID, err := rt.store.CreateSnapshot(ctx, safetySnap)
	if err != nil {
		t.Fatalf("CreateSnapshot(safety): %v", err)
	}
	targetSnap := &models.Snapshot{ShareName: shareName, State: models.StateReady, MetadataEngine: "memory"}
	targetID, err := rt.store.CreateSnapshot(ctx, targetSnap)
	if err != nil {
		t.Fatalf("CreateSnapshot(target): %v", err)
	}

	// Plant a restore marker naming both (mimics an in-flight restore).
	if merr := rt.store.PutRestoreMarker(ctx, &models.RestoreMarker{
		ShareName:        shareName,
		TargetSnapshotID: targetID,
		SafetySnapshotID: safetyID,
		Step:             models.RestoreStepStarted,
	}); merr != nil {
		t.Fatalf("PutRestoreMarker: %v", merr)
	}

	// Deleting the safety snap is refused with the typed sentinel.
	if derr := rt.DeleteSnapshot(ctx, shareName, safetyID); !errors.Is(derr, models.ErrSnapshotMarkerProtected) {
		t.Fatalf("DeleteSnapshot(safety) err = %v, want errors.Is(ErrSnapshotMarkerProtected)", derr)
	}
	// Deleting the target snap is refused too.
	if derr := rt.DeleteSnapshot(ctx, shareName, targetID); !errors.Is(derr, models.ErrSnapshotMarkerProtected) {
		t.Fatalf("DeleteSnapshot(target) err = %v, want errors.Is(ErrSnapshotMarkerProtected)", derr)
	}

	// Both rows must still exist (deletes refused) — rollback still possible.
	if _, gerr := rt.store.GetSnapshot(ctx, shareName, safetyID); gerr != nil {
		t.Fatalf("safety row deleted despite marker protection: %v", gerr)
	}
	if _, gerr := rt.store.GetSnapshot(ctx, shareName, targetID); gerr != nil {
		t.Fatalf("target row deleted despite marker protection: %v", gerr)
	}

	// Clearing the marker (restore complete) releases protection.
	if derr := rt.store.DeleteRestoreMarker(ctx, shareName); derr != nil {
		t.Fatalf("DeleteRestoreMarker: %v", derr)
	}
	if derr := rt.DeleteSnapshot(ctx, shareName, safetyID); derr != nil {
		t.Fatalf("DeleteSnapshot(safety) after marker cleared: %v", derr)
	}
}

// TestRuntimeDeleteSnapshot_NotFound asserts ErrSnapshotNotFound from the
// store propagates verbatim and the on-disk wipe step is NOT attempted.
func TestRuntimeDeleteSnapshot_NotFound(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
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
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
	defer cancel()

	// Includes separator-less traversal values (".", "..") that a bare
	// separator check would miss but filepath.Join would clean into the
	// snapshots/ parent or the share data dir — these must be rejected before
	// any RemoveAll. snapID is validated as a strict UUID.
	for _, bad := range []string{"../escape", "a/b", `a\b`, ".", "..", "not-a-uuid", ""} {
		err := rt.DeleteSnapshot(ctx, "alpha", bad)
		if !errors.Is(err, models.ErrSnapshotNotFound) {
			t.Fatalf("DeleteSnapshot(%q) err = %v, want ErrSnapshotNotFound", bad, err)
		}
	}
}
