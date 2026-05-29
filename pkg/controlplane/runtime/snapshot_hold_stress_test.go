package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// newConcurrentSnapshotRuntime returns a runtime backed by a file-backed
// SQLite store. Unlike the shared ":memory:" helper, a file DB tolerates
// the many concurrent connections the churn/mark goroutines open; an
// in-memory DB drops its schema once contention forces a second
// connection.
func newConcurrentSnapshotRuntime(t *testing.T) *Runtime {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "controlplane.db")
	s, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: dbPath},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return New(s)
}

// writeManifestForStress is the goroutine-safe (no *testing.T) manifest
// writer used by the churn workers — require.* must not run off the test
// goroutine, so errors are returned for the caller to record.
func writeManifestForStress(path string, hashes []blockstore.ContentHash) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	hs := blockstore.NewHashSet(len(hashes))
	for _, h := range hashes {
		hs.Add(h)
	}
	return snapshot.WriteManifestAtomic(path, hs)
}

// TestSnapshotHoldProvider_StressGCvsDeleteUnderChurn stresses the
// GC-mark-vs-snapshot-delete interaction (#813). It drives, concurrently:
//
//   - mark goroutines: each builds a FRESH provider via snapshotHoldForRemote
//     (mirroring blockgc.go, which constructs a new provider per GC run) and
//     calls HeldHashes, collecting the union of held hashes.
//   - churn goroutines: create transient ready snapshots (with manifests)
//     then DeleteSnapshot them — the real write path, which Lock-s the
//     per-share delete mutex.
//
// Correctness assertions:
//
//  1. No held block ever vanishes from a mark while a ready snapshot still
//     references it. A fixed set of "pinned" snapshots is created up front
//     and NEVER deleted; every HeldHashes scan that completes MUST observe
//     every pinned hash. If the delete lock failed to serialize across
//     provider instances (the #701 finding), a concurrent delete of a
//     transient snapshot could let a mark observe a partially-removed
//     manifest set — but pinned hashes must remain rock-solid regardless.
//  2. No leaked holds after delete: once all transient snapshots are
//     deleted, a final HeldHashes returns EXACTLY the pinned hashes.
//
// The mutating goroutines all run a fresh provider's AcquireDeleteLock
// (via DeleteSnapshot -> snapshotDeleteLock) against the SAME shared
// per-share mutex the mark side RLocks, so -race plus a missing held hash
// both surface a scoping regression.
func TestSnapshotHoldProvider_StressGCvsDeleteUnderChurn(t *testing.T) {
	rt := newConcurrentSnapshotRuntime(t)

	// Pinned share: snapshots created once, never deleted. Each owns a
	// unique hash that must appear in EVERY completed mark scan.
	const pinnedShare = "pinned"
	pinnedDir := t.TempDir()
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: pinnedShare, MetadataStore: "memory"})
	require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting(pinnedShare, pinnedDir))

	const pinnedCount = 8
	pinnedSet := make(map[blockstore.ContentHash]struct{}, pinnedCount)
	pinnedHashes := make([]blockstore.ContentHash, 0, pinnedCount)
	for i := 0; i < pinnedCount; i++ {
		h := hashAll(byte(0x10 + i))
		id := snapshotHoldCreateReady(t, rt, pinnedShare)
		snapshotHoldWriteManifest(t,
			(&models.Snapshot{ID: id}).ManifestPath(pinnedDir),
			[]blockstore.ContentHash{h})
		pinnedHashes = append(pinnedHashes, h)
		pinnedSet[h] = struct{}{}
	}

	// Churn shares: one per worker. A partial unique index allows at most
	// one creating row per share, so concurrent churners must each own a
	// distinct share. The mark provider scopes ALL shares, so each
	// churner's DeleteSnapshot Lock-s a share mutex the mark side RLocks —
	// exactly the cross-instance serialization under test.
	const churners = 4
	churnShares := make([]string, churners)
	churnDirs := make([]string, churners)
	allShares := []string{pinnedShare}
	for c := 0; c < churners; c++ {
		name := fmt.Sprintf("churn-%d", c)
		dir := t.TempDir()
		rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: name, MetadataStore: "memory"})
		require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting(name, dir))
		churnShares[c] = name
		churnDirs[c] = dir
		allShares = append(allShares, name)
	}

	iters := 400
	markers := 4
	if testing.Short() {
		iters = 40
	}

	var (
		markWg   sync.WaitGroup
		churnWg  sync.WaitGroup
		failed   atomic.Bool
		firstErr atomic.Pointer[string]
		stop     atomic.Bool
	)
	recordErr := func(msg string) {
		failed.Store(true)
		s := msg
		firstErr.CompareAndSwap(nil, &s)
	}

	// Mark goroutines: each builds a FRESH provider per scan (mirroring
	// blockgc.go's per-GC-run construction) and asserts every pinned hash
	// is present in the union across all shares.
	for m := 0; m < markers; m++ {
		markWg.Add(1)
		go func() {
			defer markWg.Done()
			for !stop.Load() {
				provider := rt.snapshotHoldForRemote(allShares)
				seen := make(map[blockstore.ContentHash]struct{}, pinnedCount*2)
				err := provider.HeldHashes(context.Background(), "remote-1", allShares,
					func(h blockstore.ContentHash) error {
						seen[h] = struct{}{}
						return nil
					})
				if err != nil {
					recordErr("HeldHashes error: " + err.Error())
					return
				}
				for _, h := range pinnedHashes {
					if _, ok := seen[h]; !ok {
						recordErr(fmt.Sprintf("pinned hash %x missing from mark scan", h[:4]))
						return
					}
				}
			}
		}()
	}

	// Churn goroutines: create + delete transient ready snapshots, each on
	// its own share.
	for c := 0; c < churners; c++ {
		churnWg.Add(1)
		go func(worker int) {
			defer churnWg.Done()
			ctx := context.Background()
			share := churnShares[worker]
			dir := churnDirs[worker]
			h := hashAll(byte(0x80 + worker))
			for i := 0; i < iters; i++ {
				if failed.Load() {
					return
				}
				id, err := rt.store.CreateSnapshot(ctx, &models.Snapshot{
					ShareName:      share,
					State:          models.StateCreating,
					MetadataEngine: "sqlite",
				})
				if err != nil {
					recordErr("CreateSnapshot: " + err.Error())
					return
				}
				if err := rt.store.UpdateSnapshotState(ctx, share, id, models.StateReady); err != nil {
					recordErr("UpdateSnapshotState: " + err.Error())
					return
				}
				manifestPath := (&models.Snapshot{ID: id}).ManifestPath(dir)
				if err := writeManifestForStress(manifestPath, []blockstore.ContentHash{h}); err != nil {
					recordErr("write manifest: " + err.Error())
					return
				}
				if err := rt.DeleteSnapshot(ctx, share, id); err != nil {
					recordErr("DeleteSnapshot: " + err.Error())
					return
				}
			}
		}(c)
	}

	// Churn drives the test length; once it drains, stop the markers.
	churnWg.Wait()
	stop.Store(true)
	markWg.Wait()

	if failed.Load() {
		if p := firstErr.Load(); p != nil {
			t.Fatalf("stress failure: %s", *p)
		}
		t.Fatal("stress failure (no message captured)")
	}

	// No leaked holds: every transient snapshot deleted, so a final mark
	// across all shares returns EXACTLY the pinned set.
	provider := rt.snapshotHoldForRemote(allShares)
	final := make(map[blockstore.ContentHash]struct{})
	require.NoError(t, provider.HeldHashes(context.Background(), "remote-1", allShares,
		func(h blockstore.ContentHash) error {
			final[h] = struct{}{}
			return nil
		}))
	require.Len(t, final, pinnedCount, "post-churn mark must hold exactly the pinned hashes")
	for h := range final {
		_, ok := pinnedSet[h]
		require.Truef(t, ok, "post-churn mark holds an unexpected (leaked) hash %x", h[:4])
	}

	// Every transient snapshot row on every churn share must be gone.
	for _, share := range churnShares {
		snaps, err := rt.store.ListSnapshots(context.Background(), share)
		require.NoError(t, err)
		require.Emptyf(t, snaps, "share %q must have no snapshot rows after churn", share)
	}
}

// TestSnapshotHoldProvider_DeleteLockSerializesAcrossInstances directly
// pins the #701 finding: AcquireDeleteLock on one provider instance must
// block HeldHashes on a DIFFERENT instance scoped to the same share,
// because both borrow the SAME per-share mutex from snapshotDeleteLock.
//
// If the lock were per-instance, the two would not serialize and the
// HeldHashes goroutine would complete while the delete lock is still held.
func TestSnapshotHoldProvider_DeleteLockSerializesAcrossInstances(t *testing.T) {
	const shareName = "lockscope"

	rt := newConcurrentSnapshotRuntime(t)
	localStoreDir := t.TempDir()
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          shareName,
		MetadataStore: "memory",
	})
	require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir))

	id := snapshotHoldCreateReady(t, rt, shareName)
	snapshotHoldWriteManifest(t,
		(&models.Snapshot{ID: id}).ManifestPath(localStoreDir),
		[]blockstore.ContentHash{hashAll(0x42)})

	// Provider A acquires the delete lock (write side).
	providerA := rt.snapshotHoldForRemote([]string{shareName}).(*SnapshotHoldProvider)
	release := providerA.AcquireDeleteLock(shareName)

	// Provider B (a DIFFERENT instance) tries to HeldHashes (read side).
	// It must block until A releases — proving a shared, not per-instance,
	// lock.
	providerB := rt.snapshotHoldForRemote([]string{shareName})
	markDone := make(chan error, 1)
	go func() {
		markDone <- providerB.HeldHashes(context.Background(), "remote-1", []string{shareName},
			func(blockstore.ContentHash) error { return nil })
	}()

	select {
	case err := <-markDone:
		t.Fatalf("HeldHashes on provider B completed (err=%v) while provider A held the delete lock — lock is per-instance, not shared (#701 regression)", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: B is blocked on the shared write lock.
	}

	release()

	select {
	case err := <-markDone:
		require.NoError(t, err, "HeldHashes must succeed once the delete lock is released")
	case <-time.After(2 * time.Second):
		t.Fatal("HeldHashes did not proceed after the delete lock was released")
	}
}
