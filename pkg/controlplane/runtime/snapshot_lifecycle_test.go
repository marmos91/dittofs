package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// lifecycleFixture owns every moving part of the end-to-end snapshot
// lifecycle test: the Runtime, the per-share local-store dir on disk, the
// memory remote store that holds the four CAS objects, and the four
// content hashes the sub-tests reason about.
//
// Sub-tests share state by design — the ordering of "ready preserves",
// "deletion releases", and "RemoveShare cleans" matches the documented
// snapshot lifecycle and is the load-bearing scenario this file locks in.
type lifecycleFixture struct {
	rt            *Runtime
	shareName     string
	localStoreDir string
	remote        *remotememory.Store
	metaStore     *metadatamemory.MemoryMetadataStore
	snapID        string

	// Distinct first byte per hash → distinct cas/XX prefixes; aids
	// readability when the sweep walks the namespace.
	hLive1  blockstore.ContentHash // referenced by live FileBlocks
	hLive2  blockstore.ContentHash // referenced by live FileBlocks
	hSnap   blockstore.ContentHash // referenced ONLY by the snapshot manifest
	hOrphan blockstore.ContentHash // referenced by nothing
}

// setupSnapshotLifecycle wires together everything the three sub-tests need.
// The runtime is built with the same composition the production blockgc path
// uses: in-memory SQLite control-plane store, memory metadata store, and a
// memory remote.RemoteStore bound through the test-only setShareRemoteForTest
// hook so DistinctRemoteStores surfaces it to RunBlockGC.
//
// localStoreDir is set explicitly via SetLocalStoreDirForTesting because the
// memory backend's AddShare path does not derive one — the snapshot hold
// provider relies on this dir to locate the on-disk manifest.
func setupSnapshotLifecycle(t *testing.T) *lifecycleFixture {
	t.Helper()
	ctx := context.Background()

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("memory", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	shareName := "data"
	if err := rt.AddShare(ctx, &ShareConfig{
		Name:          shareName,
		MetadataStore: "memory",
		Enabled:       true,
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	localStoreDir := t.TempDir()
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })
	// Push LastModified far enough into the past that the engine's default
	// grace TTL does not preserve any of the seeded objects on either GC pass.
	rs.SetNowFnForTest(func() time.Time { return time.Now().Add(-2 * time.Hour) })

	rt.setShareRemoteForTest(shareName, rs)

	fx := &lifecycleFixture{
		rt:            rt,
		shareName:     shareName,
		localStoreDir: localStoreDir,
		remote:        rs,
		metaStore:     metaStore,
		hLive1:        hashAll(0x11),
		hLive2:        hashAll(0x22),
		hSnap:         hashAll(0x33),
		hOrphan:       hashAll(0x44),
	}

	// Live metadata: one FileBlock per live hash. EnumerateFileBlocks will
	// stream exactly these two into the GC live set on every mark phase.
	mustPutBlock(t, metaStore, "live/1", fx.hLive1)
	mustPutBlock(t, metaStore, "live/2", fx.hLive2)

	// Seed the remote with all four CAS objects.
	mustPutRemote(t, rs, fx.hLive1, []byte("live-1-payload"))
	mustPutRemote(t, rs, fx.hLive2, []byte("live-2-payload"))
	mustPutRemote(t, rs, fx.hSnap, []byte("snap-only-payload"))
	mustPutRemote(t, rs, fx.hOrphan, []byte("orphan-payload"))

	return fx
}

// TestSnapshotLifecycleVsGC drives the snapshot lifecycle against the block
// GC in three sequential phases:
//
//  1. With a ready snapshot whose manifest covers hSnap, GC must preserve
//     hSnap alongside the two live hashes and collect only hOrphan.
//  2. After the snapshot row + on-disk directory are deleted, GC must now
//     collect hSnap as a genuine orphan.
//  3. RemoveShare must wipe the entire <localStoreDir>/snapshots/ tree
//     even when ready rows are still present in the DB.
//
// All three phases share one fixture — the order matters because each
// phase mutates state for the next.
func TestSnapshotLifecycleVsGC(t *testing.T) {
	ctx := context.Background()
	fx := setupSnapshotLifecycle(t)

	t.Run("snapshot ready preserves held block", func(t *testing.T) {
		// Snapshot lifecycle scenario: build a manifest containing one
		// live hash (hLive1) AND hSnap. hSnap diverges from live metadata
		// to model the canonical "snapshot taken at T0, file deleted at
		// T1" use case — without it the test would not distinguish
		// "live FileBlock hash" from "snapshot-held hash". The Backup
		// call below proves the Backupable wiring is reachable; the
		// manifest itself is constructed explicitly so hSnap appears
		// regardless of what live metadata Backup happens to extract.
		//
		// Real callers persist Backup's bytes into metadata.dump; this
		// test discards them — only the GC mark phase consumes the
		// manifest, and the manifest is written via WriteManifestAtomic
		// below.
		if _, err := backupAndDiscard(ctx, fx.metaStore); err != nil {
			t.Fatalf("metadata Backup: %v", err)
		}

		manifestHS := blockstore.NewHashSet(2)
		manifestHS.Add(fx.hLive1)
		manifestHS.Add(fx.hSnap)

		snapID, err := fx.rt.store.CreateSnapshot(ctx, &models.Snapshot{
			ShareName:      fx.shareName,
			State:          models.StateCreating,
			MetadataEngine: "memory",
		})
		if err != nil {
			t.Fatalf("CreateSnapshot: %v", err)
		}
		fx.snapID = snapID

		snap := &models.Snapshot{ID: snapID}
		if err := os.MkdirAll(snap.SnapshotDir(fx.localStoreDir), 0o755); err != nil {
			t.Fatalf("MkdirAll snapshot dir: %v", err)
		}
		if err := snapshot.WriteManifestAtomic(snap.ManifestPath(fx.localStoreDir), manifestHS); err != nil {
			t.Fatalf("WriteManifestAtomic: %v", err)
		}

		if err := fx.rt.store.UpdateSnapshotState(ctx, snapID, models.StateReady); err != nil {
			t.Fatalf("UpdateSnapshotState->ready: %v", err)
		}

		stats, err := fx.rt.RunBlockGC(ctx, "", false)
		if err != nil {
			t.Fatalf("RunBlockGC: %v", err)
		}
		if stats.ErrorCount != 0 {
			t.Fatalf("ErrorCount=%d on GC pass 1; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
		}
		if stats.ObjectsSwept != 1 {
			t.Fatalf("ObjectsSwept=%d on GC pass 1, want 1 (hOrphan only)", stats.ObjectsSwept)
		}

		// Live blocks must survive.
		mustHave(t, ctx, fx.remote, fx.hLive1, "hLive1 (live FileBlock) after GC pass 1")
		mustHave(t, ctx, fx.remote, fx.hLive2, "hLive2 (live FileBlock) after GC pass 1")
		// Snapshot-only block must survive: held by the ready manifest.
		mustHave(t, ctx, fx.remote, fx.hSnap, "hSnap (snapshot-held) after GC pass 1")
		// Genuine orphan must be gone.
		mustNotHave(t, ctx, fx.remote, fx.hOrphan, "hOrphan after GC pass 1")
	})

	t.Run("snapshot deletion releases held block", func(t *testing.T) {
		if fx.snapID == "" {
			t.Fatal("snapID empty; first sub-test must run before this one")
		}

		// A snapshot delete is two halves: the DB row deletion (through
		// SnapshotStore.DeleteSnapshot) and the on-disk directory
		// cleanup. The whole-share cleanup is currently wired into
		// Service.RemoveShare; the per-snapshot delete path that pairs
		// row deletion with per-directory cleanup is a future
		// orchestration concern. Tests mimic both halves explicitly
		// here so the GC-side semantics are locked in independently.
		if err := fx.rt.store.DeleteSnapshot(ctx, fx.shareName, fx.snapID); err != nil {
			t.Fatalf("DeleteSnapshot: %v", err)
		}
		snap := &models.Snapshot{ID: fx.snapID}
		if err := os.RemoveAll(snap.SnapshotDir(fx.localStoreDir)); err != nil {
			t.Fatalf("RemoveAll snapshot dir: %v", err)
		}

		stats, err := fx.rt.RunBlockGC(ctx, "", false)
		if err != nil {
			t.Fatalf("RunBlockGC: %v", err)
		}
		if stats.ErrorCount != 0 {
			t.Fatalf("ErrorCount=%d on GC pass 2; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
		}
		if stats.ObjectsSwept != 1 {
			t.Fatalf("ObjectsSwept=%d on GC pass 2, want 1 (hSnap only)", stats.ObjectsSwept)
		}

		// Live blocks still survive.
		mustHave(t, ctx, fx.remote, fx.hLive1, "hLive1 after GC pass 2")
		mustHave(t, ctx, fx.remote, fx.hLive2, "hLive2 after GC pass 2")
		// Previously-held block must now be gone.
		mustNotHave(t, ctx, fx.remote, fx.hSnap, "hSnap after GC pass 2 (no longer held)")
	})

	t.Run("RemoveShare cleans snapshots tree", func(t *testing.T) {
		// Build a fresh ready snapshot whose on-disk directory exists.
		// RemoveShare must wipe the entire <localStoreDir>/snapshots/
		// tree even though the DB row is still present at call time
		// (the hook runs alongside registry removal; orphaned DB rows
		// are operationally harmless).
		snapID, err := fx.rt.store.CreateSnapshot(ctx, &models.Snapshot{
			ShareName:      fx.shareName,
			State:          models.StateCreating,
			MetadataEngine: "memory",
		})
		if err != nil {
			t.Fatalf("CreateSnapshot: %v", err)
		}
		snap := &models.Snapshot{ID: snapID}
		if err := os.MkdirAll(snap.SnapshotDir(fx.localStoreDir), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		hs := blockstore.NewHashSet(1)
		hs.Add(fx.hLive1)
		if err := snapshot.WriteManifestAtomic(snap.ManifestPath(fx.localStoreDir), hs); err != nil {
			t.Fatalf("WriteManifestAtomic: %v", err)
		}
		if err := fx.rt.store.UpdateSnapshotState(ctx, snapID, models.StateReady); err != nil {
			t.Fatalf("UpdateSnapshotState->ready: %v", err)
		}

		snapshotsRoot := snap.SnapshotDir(fx.localStoreDir) // tree we expect gone
		if _, err := os.Stat(snapshotsRoot); err != nil {
			t.Fatalf("precondition: snapshot dir must exist before RemoveShare, stat err=%v", err)
		}

		if err := fx.rt.RemoveShare(fx.shareName); err != nil {
			t.Fatalf("RemoveShare: %v", err)
		}

		// Parent <localStoreDir>/snapshots/ must be gone.
		if _, err := os.Stat(snapshotsRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("after RemoveShare: snapshot tree should be gone, stat err=%v", err)
		}
	})
}

// hashAll fills every byte of a ContentHash with seed for deterministic,
// human-readable hash literals in test output.
func hashAll(seed byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	for i := range h {
		h[i] = seed
	}
	return h
}

// mustPutBlock seeds a finalized FileBlock keyed by hash on the metadata
// store. State=Remote so the engine treats it as live during mark.
func mustPutBlock(t *testing.T, st metadata.MetadataStore, id string, h blockstore.ContentHash) {
	t.Helper()
	if err := st.Put(context.Background(), &blockstore.FileBlock{
		ID:            id,
		Hash:          h,
		State:         blockstore.BlockStateRemote,
		BlockStoreKey: blockstore.FormatCASKey(h),
		LocalPath:     "/cache/" + id,
		DataSize:      64,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("Put FileBlock(%s): %v", id, err)
	}
}

// mustPutRemote seeds a CAS object on the remote keyed by hash.
func mustPutRemote(t *testing.T, rs *remotememory.Store, h blockstore.ContentHash, data []byte) {
	t.Helper()
	if err := rs.Put(context.Background(), h, data); err != nil {
		t.Fatalf("remote.Put(%x): %v", h[:4], err)
	}
}

// mustHave asserts the remote currently holds h, failing with msg if not.
func mustHave(t *testing.T, ctx context.Context, rs *remotememory.Store, h blockstore.ContentHash, msg string) {
	t.Helper()
	if _, err := rs.Head(ctx, h); err != nil {
		t.Fatalf("%s: Head err=%v (expected object present)", msg, err)
	}
}

// mustNotHave asserts the remote does NOT hold h, failing with msg if it does.
func mustNotHave(t *testing.T, ctx context.Context, rs *remotememory.Store, h blockstore.ContentHash, msg string) {
	t.Helper()
	if _, err := rs.Head(ctx, h); err == nil {
		t.Fatalf("%s: object still present (expected deleted)", msg)
	}
}

// backupAndDiscard runs the memory backend's Backupable.Backup, discards the
// bytes (real callers persist them to metadata.dump), and returns just the
// HashSet for assertion. Surfaces the same call path the production snapshot
// creator uses without coupling the test to manifest contents derived from
// live metadata.
func backupAndDiscard(ctx context.Context, st *metadatamemory.MemoryMetadataStore) (*blockstore.HashSet, error) {
	var buf bytes.Buffer
	return st.Backup(ctx, &buf)
}
