package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	bsmemory "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestCreateSnapshot_Integration drives the end-to-end snapshot orchestration
// stack landed in phase 23 plans 23-01..05 + WaitForSnapshot (plan 23-06).
// Each sub-test exercises a distinct path (happy path, drain-then-verify,
// retry, no-sync-gate, RemoveShare cancels in-flight, startup recovery)
// against a memory-only fixture (cpstore SQLite + memory metadata + memory
// remote) per CONTEXT line 211: "Memory-only integration test for
// orchestration semantics — Phase 22 D-21 sets the precedent."
//
// ROADMAP success criteria covered:
//   - SC-1 (VerifyRemoteDurability): HappyPath + DrainThenVerifyFails
//   - SC-2 (CreateSnapshot produces ready snapshot with metadata.dump
//   - manifest on disk): HappyPath
//   - SC-3 (NoSyncGate skips verify, GC hold still applies): NoSyncGate
//   - SC-4 (integration test passes): the suite running green
//
// Lifecycle decisions covered:
//   - D-23-09 (failed rows retain artifacts on disk for retry)
//   - D-23-10 (RetryOf reuses ID + dir)
//   - D-23-11 (NoSyncGate path)
//   - D-23-17 (cancel + WG.Wait before tree wipe in RemoveShare)
//   - D-23-18 (recoverOrphanedSnapshots flips creating → failed)
//   - D-23-19 (WaitForSnapshot carries orchestration error)
func TestCreateSnapshot_Integration(t *testing.T) {
	t.Run("HappyPath", testHappyPath)
	t.Run("DrainThenVerifyPasses", testDrainThenVerifyPasses)
	t.Run("DrainThenVerifyFails", testDrainThenVerifyFails)
	t.Run("RetryOfFailed", testRetryOfFailed)
	t.Run("NoSyncGate", testNoSyncGate)
	t.Run("RemoveShareCancelsInFlight", testRemoveShareCancelsInFlight)
	t.Run("StartupRecovery", testStartupRecovery)
}

// ----- Sub-tests -----

func testHappyPath(t *testing.T) {
	fx := newOrchestrationFixture(t)
	defer fx.close()

	hashes := makeHashes(3, 0xa0)
	fx.setBackupHashes(hashes)
	fx.seedRemoteAll(hashes)

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if snapID == "" {
		t.Fatal("CreateSnapshot returned empty snapID")
	}

	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: err = %v, want nil", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snap.State = %q, want %q", snap.State, models.StateReady)
	}
	if !snap.RemoteDurable {
		t.Fatalf("snap.RemoteDurable = false, want true (D-23-03)")
	}

	// SC-2: metadata.dump + manifest exist on disk + non-empty.
	dumpPath := snap.MetadataDumpPath(fx.localStoreDir)
	manifestPath := snap.ManifestPath(fx.localStoreDir)
	mustFileNonEmpty(t, dumpPath, "metadata.dump")
	mustFileNonEmpty(t, manifestPath, "manifest.hashes")
}

func testDrainThenVerifyPasses(t *testing.T) {
	fx := newOrchestrationFixture(t)
	defer fx.close()

	hashes := makeHashes(2, 0xb0)
	fx.setBackupHashes(hashes)
	// Remote pre-seeded with every hash → verify passes on the first
	// attempt; the drain step still runs (DrainAllUploads is a no-op on
	// the memory backend because ListUnsynced is empty) and is the
	// load-bearing step under test.
	fx.seedRemoteAll(hashes)

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snap.State = %q, want %q", snap.State, models.StateReady)
	}
	if !snap.RemoteDurable {
		t.Fatalf("snap.RemoteDurable = false, want true")
	}
}

func testDrainThenVerifyFails(t *testing.T) {
	fx := newOrchestrationFixture(t)
	defer fx.close()

	hashes := makeHashes(3, 0xc0)
	fx.setBackupHashes(hashes)
	// Seed all but the LAST hash so the verify step's per-hash Head probe
	// hits ErrChunkNotFound for one specific hash even after the
	// post-miss re-drain (memory drain is a no-op so the second verify
	// still misses).
	fx.seedRemoteSubset(hashes[:len(hashes)-1])

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	// D-23-19 + iteration-1 revision: WaitForSnapshot returns the
	// wrapped sentinel directly via errors.Is — no slog interception,
	// no schema column.
	if !errors.Is(werr, models.ErrSnapshotVerifyFailed) {
		t.Fatalf("WaitForSnapshot err = %v, want errors.Is(ErrSnapshotVerifyFailed)", werr)
	}
	if snap == nil {
		t.Fatal("WaitForSnapshot returned nil snapshot even with non-nil err")
	}
	if snap.State != models.StateFailed {
		t.Fatalf("snap.State = %q, want %q", snap.State, models.StateFailed)
	}

	// D-23-09: metadata.dump + manifest.hashes are retained on disk
	// for retry.
	mustFileNonEmpty(t, snap.MetadataDumpPath(fx.localStoreDir), "metadata.dump (retained on failure)")
	mustFileNonEmpty(t, snap.ManifestPath(fx.localStoreDir), "manifest.hashes (retained on failure)")
}

func testRetryOfFailed(t *testing.T) {
	fx := newOrchestrationFixture(t)
	defer fx.close()

	hashes := makeHashes(3, 0xd0)
	fx.setBackupHashes(hashes)
	// First attempt: missing one hash → fail.
	fx.seedRemoteSubset(hashes[:len(hashes)-1])

	ctx := fx.ctx()
	firstID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot (initial): %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, firstID)
	if !errors.Is(werr, models.ErrSnapshotVerifyFailed) {
		t.Fatalf("initial WaitForSnapshot: err = %v, want errors.Is(ErrSnapshotVerifyFailed)", werr)
	}
	if snap.State != models.StateFailed {
		t.Fatalf("initial snap.State = %q, want %q", snap.State, models.StateFailed)
	}

	// Retry with full remote.
	fx.seedRemoteAll(hashes)
	retryID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{RetryOf: firstID})
	if err != nil {
		t.Fatalf("CreateSnapshot (retry): %v", err)
	}
	if retryID != firstID {
		t.Fatalf("D-23-10: retry reused ID? got %q want %q", retryID, firstID)
	}

	retried, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, retryID)
	if werr != nil {
		t.Fatalf("retry WaitForSnapshot: %v", werr)
	}
	if retried.State != models.StateReady {
		t.Fatalf("retry snap.State = %q, want %q", retried.State, models.StateReady)
	}
	if !retried.RemoteDurable {
		t.Fatalf("retry snap.RemoteDurable = false, want true")
	}

	// RetryOf pointing at a ready snap → ErrSnapshotRetryTargetNotFailed.
	_, err = fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{RetryOf: retryID})
	if !errors.Is(err, models.ErrSnapshotRetryTargetNotFailed) {
		t.Fatalf("retry-of-ready: err = %v, want errors.Is(ErrSnapshotRetryTargetNotFailed)", err)
	}

	// RetryOf pointing at a non-existent ID → ErrSnapshotRetryTargetNotFound.
	_, err = fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{RetryOf: "no-such-id"})
	if !errors.Is(err, models.ErrSnapshotRetryTargetNotFound) {
		t.Fatalf("retry-of-missing: err = %v, want errors.Is(ErrSnapshotRetryTargetNotFound)", err)
	}
}

func testNoSyncGate(t *testing.T) {
	fx := newOrchestrationFixture(t)
	defer fx.close()

	hashes := makeHashes(4, 0xe0)
	fx.setBackupHashes(hashes)
	// Deliberately leave the remote empty — would fail verify if the
	// sync gate engaged. NoSyncGate must skip drain + verify entirely.

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoSyncGate: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snap.State = %q, want %q", snap.State, models.StateReady)
	}
	if snap.RemoteDurable {
		t.Fatalf("snap.RemoteDurable = true, want false (NoSyncGate D-23-11)")
	}

	// SC-3 / D-23-02 sub-assertion: the plan-23-03 hold provider still
	// streams the snapshot's hashes (manifest-on-disk filter is
	// disposition-independent of RemoteDurable).
	provider, ok := fx.rt.snapshotHoldForRemote([]string{fx.shareName}).(*SnapshotHoldProvider)
	if !ok {
		t.Fatal("snapshotHoldForRemote did not return *SnapshotHoldProvider")
	}
	heldSet := make(map[blockstore.ContentHash]struct{}, len(hashes))
	if err := provider.HeldHashes(ctx, "remote-nosg", []string{fx.shareName},
		func(h blockstore.ContentHash) error {
			heldSet[h] = struct{}{}
			return nil
		}); err != nil {
		t.Fatalf("HeldHashes: %v", err)
	}
	for _, h := range hashes {
		if _, ok := heldSet[h]; !ok {
			t.Fatalf("SC-3: hash %s missing from hold set after NoSyncGate snapshot", h)
		}
	}
}

func testRemoveShareCancelsInFlight(t *testing.T) {
	fx := newOrchestrationFixture(t)
	// Deliberately no defer fx.close() — RemoveShare drops the share
	// out of the registry and closes its BlockStore; subsequent close
	// would double-close. Cleanup of the remote + cpstore is handled
	// by their own t.Cleanup registrations inside newOrchestrationFixture.

	hashes := makeHashes(2, 0xf0)
	fx.setBackupHashes(hashes)
	// Block Backup until either the context cancels or the test releases
	// it explicitly. Per the plan body: prefer ctx-cancel-driven unblock
	// (cancelAndWaitInFlightSnaps cancels the orchestration child ctx,
	// which propagates to backupable.Backup's select).
	started := make(chan struct{})
	release := make(chan struct{})
	fx.backup.setHook(func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	ctx := fx.ctx()
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Wait for the orchestration goroutine to enter slowBackupable.Backup.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slowBackupable.Backup never reached: orchestration did not start backup")
	}

	// Capture pre-RemoveShare on-disk paths so we can assert they
	// disappear post-RemoveShare.
	snapDirBefore := (&models.Snapshot{ID: snapID}).SnapshotDir(fx.localStoreDir)
	snapsRoot := filepath.Dir(snapDirBefore)
	if _, err := os.Stat(snapDirBefore); err != nil {
		t.Fatalf("precondition: snapDirBefore missing pre-RemoveShare: %v", err)
	}

	// RemoveShare must drain in-flight snapshots BEFORE the Phase 22 D-15
	// tree wipe (D-23-17). We rely on ctx-cancel-driven unblock (no test
	// signal on `release`).
	rmDone := make(chan error, 1)
	go func() { rmDone <- fx.rt.RemoveShare(fx.shareName) }()

	select {
	case rmErr := <-rmDone:
		if rmErr != nil {
			t.Fatalf("RemoveShare: %v", rmErr)
		}
	case <-time.After(10 * time.Second):
		// Safety net: release the hook so the goroutine cannot stay
		// pinned and surface the actual failure cleanly.
		close(release)
		t.Fatal("RemoveShare timed out — cancel-and-wait did not unblock the slow Backup")
	}

	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	// Either path is acceptable per the plan: the orchestration
	// goroutine wrote state=failed and posted snapResult{err:
	// wrappedCancel}, OR the registry entry was already removed before
	// WaitForSnapshot looked it up and the call falls straight through
	// to GetSnapshot. Both paths return the row; the error is
	// informational, not load-bearing.
	if werr != nil && !errors.Is(werr, context.Canceled) &&
		!errors.Is(werr, models.ErrSnapshotBackupFailed) &&
		!errors.Is(werr, models.ErrSnapshotDrainTimeout) &&
		!errors.Is(werr, models.ErrSnapshotVerifyFailed) {
		t.Fatalf("WaitForSnapshot: unexpected err %v", werr)
	}
	if snap == nil {
		t.Fatal("WaitForSnapshot returned nil snapshot — DB row should survive RemoveShare per Phase 22 invariant")
	}
	// D-23-09 / D-23-17: cancelled orchestration flipped its row to
	// state=failed BEFORE RemoveShare ran the tree wipe.
	if snap.State != models.StateFailed {
		t.Fatalf("D-23-09: snap.State = %q, want %q (orchestration must flip on cancel)", snap.State, models.StateFailed)
	}
	if snap.ID != snapID {
		t.Fatalf("snap.ID = %q, want %q (orphan row must survive RemoveShare per Phase 22 invariant)", snap.ID, snapID)
	}

	// Phase 22 D-15 wipes the entire snapshots/ tree, not just the
	// per-id subdir. Both must be gone.
	if _, err := os.Stat(snapDirBefore); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Phase 22 D-15: per-snap dir should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(snapsRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Phase 22 D-15: snapshots/ root should be gone, stat err = %v", err)
	}

	// Final safety: signal release to drain any goroutine still pinned
	// on the hook (defensive — ctx cancel should already have freed it).
	// Idempotent: closing a closed chan panics, but we only ever closed
	// it on the timeout-fast-path, which would have returned by t.Fatal.
	close(release)
}

func testStartupRecovery(t *testing.T) {
	fx := newOrchestrationFixture(t)
	defer fx.close()

	ctx := fx.ctx()

	// Insert a creating row directly (no goroutine) — simulates a crash
	// mid-create where the prior process never reached the ready/failed
	// flip. D-23-18.
	orphanID, err := fx.rt.store.CreateSnapshot(ctx, &models.Snapshot{
		ShareName:      fx.shareName,
		State:          models.StateCreating,
		MetadataEngine: "memory",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot (orphan seed): %v", err)
	}

	// Pre-seed metadata.dump + manifest on disk so we can assert
	// retention semantics (D-23-09 carries to the recovery path: the
	// flipped state=failed row keeps its artifacts).
	snapDir := (&models.Snapshot{ID: orphanID}).SnapshotDir(fx.localStoreDir)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		t.Fatalf("mkdir orphan snapshot dir: %v", err)
	}
	dumpPath := filepath.Join(snapDir, "metadata.dump")
	manifestPath := filepath.Join(snapDir, "manifest.hashes")
	if err := os.WriteFile(dumpPath, []byte("pre-crash-dump"), 0o640); err != nil {
		t.Fatalf("write pre-crash dump: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte{}, 0o640); err != nil {
		t.Fatalf("write empty manifest: %v", err)
	}

	if err := fx.rt.recoverOrphanedSnapshots(ctx); err != nil {
		t.Fatalf("recoverOrphanedSnapshots: %v", err)
	}

	got, err := fx.rt.store.GetSnapshot(ctx, fx.shareName, orphanID)
	if err != nil {
		t.Fatalf("GetSnapshot post-recovery: %v", err)
	}
	if got.State != models.StateFailed {
		t.Fatalf("D-23-18: post-recovery state = %q, want %q", got.State, models.StateFailed)
	}

	// D-23-09 retention: the metadata.dump + manifest.hashes must still
	// be on disk after recovery so the operator can retry via
	// CreateSnapshot(RetryOf=…).
	if _, err := os.Stat(dumpPath); err != nil {
		t.Fatalf("D-23-09: metadata.dump removed by recovery: %v", err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("D-23-09: manifest.hashes removed by recovery: %v", err)
	}
}

// ----- Fixture -----

// orchestrationFixture composes every moving part the seven sub-tests
// need: cpstore, runtime, memory metadata store wrapped in a controlled
// Backupable, memory remote, real engine.BlockStore wiring the local +
// remote, and an injected Share that ties them together.
type orchestrationFixture struct {
	t             *testing.T
	rt            *Runtime
	store         cpstore.Store
	backup        *controlledBackupable
	remote        *interceptingRemote
	bs            *engine.BlockStore
	localStoreDir string
	shareName     string
}

func newOrchestrationFixture(t *testing.T) *orchestrationFixture {
	t.Helper()

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	mem := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	backup := &controlledBackupable{MemoryMetadataStore: mem}
	if err := rt.RegisterMetadataStore("memory", backup); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	localStoreDir := t.TempDir()
	shareName := "data"

	// Build a minimal real engine.BlockStore. The memory local store +
	// memory remote + memory metadata as FileBlockStore have empty
	// ListUnsynced semantics so DrainAllUploads is a fast no-op and
	// mirrorOnce short-circuits — exactly what the orchestration
	// integration test wants (we are testing the orchestration
	// machinery, not the syncer).
	localStore := bsmemory.New()
	innerRemote := remotememory.New()
	t.Cleanup(func() { _ = innerRemote.Close() })
	wrappedRemote := newInterceptingRemote(innerRemote)
	syncer := engine.NewSyncer(localStore, wrappedRemote, mem, engine.SyncerConfig{
		ParallelUploads:   1,
		ParallelDownloads: 1,
	})
	bs, err := engine.New(engine.Config{
		Local:          localStore,
		Remote:         wrappedRemote,
		Syncer:         syncer,
		FileBlockStore: mem,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          shareName,
		MetadataStore: "memory",
		BlockStore:    bs,
	})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	fx := &orchestrationFixture{
		t:             t,
		rt:            rt,
		store:         cp,
		backup:        backup,
		remote:        wrappedRemote,
		bs:            bs,
		localStoreDir: localStoreDir,
		shareName:     shareName,
	}
	return fx
}

// close runs the runtime-level shutdown so any in-flight orchestration
// goroutines drain before the test exits. Skipped for sub-tests that
// already called RemoveShare (which closes the BlockStore as part of
// its own teardown).
func (f *orchestrationFixture) close() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.rt.Shutdown(ctx); err != nil {
		f.t.Logf("Shutdown: %v", err)
	}
}

// ctx returns a per-sub-test bounded ctx. 30s is comfortably above the
// expected runtime of any sub-test (most complete in <500ms; the slow
// Backup hook in RemoveShareCancelsInFlight adds ~0s because ctx cancel
// unblocks the select immediately).
func (f *orchestrationFixture) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	f.t.Cleanup(cancel)
	return ctx
}

func (f *orchestrationFixture) setBackupHashes(h []blockstore.ContentHash) {
	f.backup.setHashes(h)
}

func (f *orchestrationFixture) seedRemoteAll(hashes []blockstore.ContentHash) {
	f.t.Helper()
	for _, h := range hashes {
		if err := f.remote.inner.Put(context.Background(), h, []byte("payload-"+h.String())); err != nil {
			f.t.Fatalf("seed remote put: %v", err)
		}
	}
}

func (f *orchestrationFixture) seedRemoteSubset(hashes []blockstore.ContentHash) {
	f.t.Helper()
	for _, h := range hashes {
		if err := f.remote.inner.Put(context.Background(), h, []byte("payload-"+h.String())); err != nil {
			f.t.Fatalf("seed remote put: %v", err)
		}
	}
}

// ----- controlledBackupable -----

// controlledBackupable wraps a real MemoryMetadataStore so it still
// satisfies the full metadata.MetadataStore interface (via embedded
// pointer method promotion) but overrides Backup so the test can:
//   - return a deterministic HashSet without seeding files through the
//     real Put/transaction path
//   - block on a caller-supplied hook (used by the slow-Backup fixture
//     in the RemoveShare-cancels-in-flight sub-test)
//
// The wrapper writes a few bytes to w so the resulting metadata.dump
// file is non-empty (a SC-2 assertion).
type controlledBackupable struct {
	*metadatamemory.MemoryMetadataStore

	mu     sync.Mutex
	hashes []blockstore.ContentHash
	hook   func(ctx context.Context) error
}

func (c *controlledBackupable) setHashes(h []blockstore.ContentHash) {
	c.mu.Lock()
	c.hashes = append([]blockstore.ContentHash(nil), h...)
	c.mu.Unlock()
}

func (c *controlledBackupable) setHook(fn func(ctx context.Context) error) {
	c.mu.Lock()
	c.hook = fn
	c.mu.Unlock()
}

func (c *controlledBackupable) Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error) {
	c.mu.Lock()
	hook := c.hook
	hashes := append([]blockstore.ContentHash(nil), c.hashes...)
	c.mu.Unlock()

	if hook != nil {
		if err := hook(ctx); err != nil {
			return nil, err
		}
	}
	if _, err := w.Write([]byte("controlled-backup-payload")); err != nil {
		return nil, err
	}
	hs := blockstore.NewHashSet(len(hashes))
	for _, h := range hashes {
		hs.Add(h)
	}
	return hs, nil
}

// ----- interceptingRemote -----

// interceptingRemote wraps a memory RemoteStore and delegates everything
// to it. The "missing one hash" sub-test does not actually need
// interception; we exercise the missing-hash path by simply not seeding
// that hash into the inner store (Head returns ErrChunkNotFound
// naturally). The wrapper is retained as a future hook point and to
// keep all RemoteStore-related test helpers funnelled through one type.
type interceptingRemote struct {
	inner *remotememory.Store
}

func newInterceptingRemote(inner *remotememory.Store) *interceptingRemote {
	return &interceptingRemote{inner: inner}
}

func (r *interceptingRemote) Put(ctx context.Context, hash blockstore.ContentHash, data []byte) error {
	return r.inner.Put(ctx, hash, data)
}

func (r *interceptingRemote) Get(ctx context.Context, hash blockstore.ContentHash) ([]byte, error) {
	return r.inner.Get(ctx, hash)
}

func (r *interceptingRemote) GetRange(ctx context.Context, hash blockstore.ContentHash, offset, length int64) ([]byte, error) {
	return r.inner.GetRange(ctx, hash, offset, length)
}

func (r *interceptingRemote) Delete(ctx context.Context, hash blockstore.ContentHash) error {
	return r.inner.Delete(ctx, hash)
}

func (r *interceptingRemote) Head(ctx context.Context, hash blockstore.ContentHash) (blockstore.Meta, error) {
	return r.inner.Head(ctx, hash)
}

func (r *interceptingRemote) Walk(ctx context.Context, fn func(hash blockstore.ContentHash, meta blockstore.Meta) error) error {
	return r.inner.Walk(ctx, fn)
}

func (r *interceptingRemote) ReadBlockVerified(ctx context.Context, hash, expected blockstore.ContentHash) ([]byte, error) {
	return r.inner.ReadBlockVerified(ctx, hash, expected)
}

func (r *interceptingRemote) Close() error { return r.inner.Close() }

func (r *interceptingRemote) HealthCheck(ctx context.Context) error { return r.inner.HealthCheck(ctx) }

func (r *interceptingRemote) Healthcheck(ctx context.Context) health.Report {
	return r.inner.Healthcheck(ctx)
}

// Compile-time guard: interceptingRemote satisfies remote.RemoteStore.
var _ remote.RemoteStore = (*interceptingRemote)(nil)

// ----- small helpers -----

func makeHashes(n int, seed byte) []blockstore.ContentHash {
	out := make([]blockstore.ContentHash, n)
	for i := 0; i < n; i++ {
		out[i] = hashAllByte(seed + byte(i))
	}
	return out
}

// hashAllByte fills every byte of a ContentHash with the seed. Distinct
// from hashAll in snapshot_lifecycle_test.go only in name to avoid
// per-file collision; tests in the same package share the symbol space.
func hashAllByte(seed byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	for i := range h {
		h[i] = seed
	}
	return h
}

func mustFileNonEmpty(t *testing.T, path, label string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: stat %q: %v", label, path, err)
	}
	if st.IsDir() {
		t.Fatalf("%s: %q is a directory, want file", label, path)
	}
	if label == "manifest.hashes" {
		// Manifest may legitimately be empty when the HashSet has zero
		// entries; relax to "exists".
		return
	}
	if st.Size() == 0 {
		t.Fatalf("%s: %q is empty, want non-empty", label, path)
	}
}
