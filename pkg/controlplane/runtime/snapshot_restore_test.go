package runtime

import (
	"context"
	"errors"
	"os"
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
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestRestoreSnapshot_Integration drives the end-to-end restore
// orchestration against a memory-only fixture (cpstore SQLite + memory
// metadata + memory remote). Each sub-test exercises one scenario —
// happy path plus the failure-mode taxonomy. The "share stays disabled"
// invariant is asserted in every sub-test that reaches a non-trivial
// code path.
func TestRestoreSnapshot_Integration(t *testing.T) {
	t.Run("HappyPath", testRestoreHappyPath)
	t.Run("EnabledShareRefuses", testRestoreEnabledShareRefuses)
	t.Run("SnapshotNotFound", testRestoreSnapshotNotFound)
	t.Run("SnapshotNotReady", testRestoreSnapshotNotReady)
	t.Run("NonDurableRefused", testRestoreNonDurableRefused)
	t.Run("AllowNonDurable", testRestoreAllowNonDurable)
	t.Run("PreVerifyFailsFast", testRestorePreVerifyFailsFast)
	t.Run("PostVerifyFails", testRestorePostVerifyFails)
	t.Run("InterruptedRestore", testRestoreInterruptedReset)
}

// ----- Sub-tests -----

func testRestoreHappyPath(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()

	// Populate 2 files with 3 unique block hashes; seed remote with all.
	files := fx.populateFiles(ctx, []string{"alpha.bin", "beta.bin"})
	fx.seedRemoteAll(fx.allHashes())

	// Create snapshot of the populated state.
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("source snap.State = %q, want %q", snap.State, models.StateReady)
	}
	if !snap.RemoteDurable {
		t.Fatalf("source snap.RemoteDurable = false, want true")
	}

	// Mutate: delete one of the files so restore has something to recover.
	deletedFile := files[0]
	if err := fx.meta.DeleteFile(ctx, deletedFile.handle); err != nil {
		t.Fatalf("delete file alpha pre-restore: %v", err)
	}

	// Restore — must complete with no error.
	if err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{}); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	// The deleted file is now present in the metadata store again.
	got, err := fx.meta.GetFile(ctx, deletedFile.handle)
	if err != nil {
		t.Fatalf("GetFile post-restore: deleted file not recovered: %v", err)
	}
	if got == nil {
		t.Fatal("GetFile post-restore: nil file")
	}

	// share stays disabled.
	enabled, err := fx.rt.sharesSvc.IsShareEnabled(fx.shareName)
	if err != nil {
		t.Fatalf("IsShareEnabled: %v", err)
	}
	if enabled {
		t.Fatal("share is enabled after RestoreSnapshot — must stay disabled")
	}

	// A safety snapshot was created (in addition to the source snap).
	snaps, err := fx.store.ListSnapshots(ctx, fx.shareName)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) < 2 {
		t.Fatalf("safety snapshot missing — ListSnapshots returned %d entries, want >=2", len(snaps))
	}
	foundSafety := false
	for _, s := range snaps {
		if s.ID != snapID {
			foundSafety = true
			break
		}
	}
	if !foundSafety {
		t.Fatal("no safety snapshot found in ListSnapshots")
	}
}

func testRestoreEnabledShareRefuses(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{shareEnabled: true})
	defer fx.close()

	ctx := fx.ctx()
	fx.populateFiles(ctx, []string{"only.bin"})
	fx.seedRemoteAll(fx.allHashes())

	// Disable temporarily to permit CreateSnapshot's localStoreDir path? No —
	// CreateSnapshot has no Enabled precondition. We CAN create snapshot
	// while share is Enabled. So create snapshot then attempt restore while
	// still Enabled.
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	preCount := fx.countFiles(ctx)

	err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrShareEnabled) {
		t.Fatalf("RestoreSnapshot err = %v, want errors.Is(ErrShareEnabled)", err)
	}

	postCount := fx.countFiles(ctx)
	if postCount != preCount {
		t.Fatalf("metadata mutated despite refusal: file count %d -> %d", preCount, postCount)
	}
}

func testRestoreSnapshotNotFound(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	fx.populateFiles(ctx, []string{"foo.bin"})

	preCount := fx.countFiles(ctx)
	err := fx.rt.RestoreSnapshot(ctx, fx.shareName, "nonexistent-snap-id", RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrSnapshotNotFound) {
		t.Fatalf("RestoreSnapshot err = %v, want errors.Is(ErrSnapshotNotFound)", err)
	}

	postCount := fx.countFiles(ctx)
	if postCount != preCount {
		t.Fatalf("metadata mutated despite refusal: file count %d -> %d", preCount, postCount)
	}
}

func testRestoreSnapshotNotReady(t *testing.T) {
	// Sub-case (a): state=failed.
	// Mechanism: seed only a subset of hashes on the remote so verify fails,
	// snapshot lands in state=failed; RestoreSnapshot should refuse with
	// ErrSnapshotStateConflict.
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	fx.populateFiles(ctx, []string{"x.bin", "y.bin"})
	all := fx.allHashes()
	// Seed all but the last so verify fails.
	fx.seedRemoteAll(all[:len(all)-1])

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if !errors.Is(werr, models.ErrSnapshotVerifyFailed) {
		t.Fatalf("expected source snap to fail verify, got %v", werr)
	}
	if snap.State != models.StateFailed {
		t.Fatalf("source snap.State = %q, want %q", snap.State, models.StateFailed)
	}

	preCount := fx.countFiles(ctx)
	err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrSnapshotStateConflict) {
		t.Fatalf("RestoreSnapshot err = %v, want errors.Is(ErrSnapshotStateConflict)", err)
	}
	postCount := fx.countFiles(ctx)
	if postCount != preCount {
		t.Fatalf("metadata mutated despite refusal: file count %d -> %d", preCount, postCount)
	}
}

func testRestoreNonDurableRefused(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	fx.populateFiles(ctx, []string{"nd.bin"})
	// Do NOT seed remote — NoSyncGate skips verify so create still succeeds.

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
		t.Fatalf("snap.RemoteDurable = true, want false (NoSyncGate)")
	}

	preCount := fx.countFiles(ctx)
	err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrSnapshotNotDurable) {
		t.Fatalf("RestoreSnapshot err = %v, want errors.Is(ErrSnapshotNotDurable)", err)
	}
	postCount := fx.countFiles(ctx)
	if postCount != preCount {
		t.Fatalf("metadata mutated despite refusal: file count %d -> %d", preCount, postCount)
	}
}

func testRestoreAllowNonDurable(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"and.bin"})

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoSyncGate: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	if snap.RemoteDurable {
		t.Fatalf("expected RemoteDurable=false")
	}

	// Manually seed remote so pre-verify + post-verify pass (the safety
	// snap is sync-gated, so its drain/verify also needs the hashes).
	fx.seedRemoteAll(fx.allHashes())

	// Mutate: delete the file.
	if err := fx.meta.DeleteFile(ctx, files[0].handle); err != nil {
		t.Fatalf("delete pre-restore: %v", err)
	}

	if err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{AllowNonDurable: true}); err != nil {
		t.Fatalf("RestoreSnapshot(AllowNonDurable): %v", err)
	}

	// Data restored.
	if _, err := fx.meta.GetFile(ctx, files[0].handle); err != nil {
		t.Fatalf("GetFile post-restore: %v", err)
	}
	// Share stays disabled.
	enabled, _ := fx.rt.sharesSvc.IsShareEnabled(fx.shareName)
	if enabled {
		t.Fatal("share enabled after restore — must stay disabled")
	}
}

func testRestorePreVerifyFailsFast(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"pv1.bin", "pv2.bin"})
	all := fx.allHashes()
	fx.seedRemoteAll(all)

	// Source snapshot must succeed (remote has every hash).
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot source: %v", werr)
	}

	// Mutate so pre-Reset state has something distinct (witness for the
	// metadata-unchanged invariant). Then arm the head-failure for ONE
	// hash on every subsequent call (threshold=0).
	if err := fx.meta.DeleteFile(ctx, files[0].handle); err != nil {
		t.Fatalf("delete pre-restore witness: %v", err)
	}
	preCount := fx.countFiles(ctx)
	fx.remote.failHashAfterCount(files[1].hashes[0], 0) // every Head() for this hash fails

	err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrRestoreVerifyFailed) {
		t.Fatalf("RestoreSnapshot err = %v, want errors.Is(ErrRestoreVerifyFailed)", err)
	}

	// Reset was NEVER called: metadata count unchanged.
	postCount := fx.countFiles(ctx)
	if postCount != preCount {
		t.Fatalf("metadata mutated despite pre-verify fail: file count %d -> %d", preCount, postCount)
	}

	// Safety snapshot was NEVER created (pre-verify fires before step 3).
	snaps, err := fx.store.ListSnapshots(ctx, fx.shareName)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].ID != snapID {
		t.Fatalf("safety snapshot created despite pre-verify fail: ListSnapshots = %d entries (want 1, just %q)", len(snaps), snapID)
	}

	// Share stays disabled.
	enabled, _ := fx.rt.sharesSvc.IsShareEnabled(fx.shareName)
	if enabled {
		t.Fatal("share enabled after pre-verify fail — must stay disabled")
	}
}

func testRestorePostVerifyFails(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	// Populate two files so we can delete one BEFORE the safety snap is
	// taken — the deleted file's hash is in the source manifest but NOT
	// in the safety snap's manifest, so we can target Head() failure at
	// it without affecting pre-verify or the safety-snap verify.
	files := fx.populateFiles(ctx, []string{"post1.bin", "post2.bin"})
	all := fx.allHashes()
	fx.seedRemoteAll(all)

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot source: %v", werr)
	}

	// Mutate AFTER snapshot create: delete files[0]. files[0].hashes[0]
	// is now only in the source manifest (the safety snap will capture
	// the mutated state without it).
	if err := fx.meta.DeleteFile(ctx, files[0].handle); err != nil {
		t.Fatalf("delete files[0]: %v", err)
	}

	// Arm head-failure for files[0].hashes[0] starting at the 2nd call.
	// Sequence:
	//   1st Head(h0) — pre-verify (source manifest contains h0). PASS.
	//   safety snap verify — does NOT call Head(h0) (h0 isn't in safety
	//     snap manifest because we deleted files[0] above).
	//   2nd Head(h0) — post-verify (restored manifest contains h0). FAIL.
	deletedHash := files[0].hashes[0]
	fx.remote.failHashAfterCount(deletedHash, 1)

	err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrRestoreVerifyFailed) {
		t.Fatalf("RestoreSnapshot err = %v, want errors.Is(ErrRestoreVerifyFailed)", err)
	}

	// Metadata WAS restored (we reached post-verify, which means Reset +
	// Restore both ran before the failure). The deleted file should now
	// be back in the store.
	if _, gerr := fx.meta.GetFile(ctx, files[0].handle); gerr != nil {
		t.Fatalf("post-restore GetFile (proves we reached post-verify): %v", gerr)
	}

	// Safety snapshot exists on disk + in DB.
	safetyID := fx.safetySnapshotID(ctx, snapID)
	if safetyID == "" {
		t.Fatal("safety snapshot ID not found in ListSnapshots")
	}
	fileExistsNonEmpty(t,
		(&models.Snapshot{ID: safetyID}).MetadataDumpPath(fx.localStoreDir),
		"safety snap dump retained on disk")

	// Share stays disabled.
	enabled, _ := fx.rt.sharesSvc.IsShareEnabled(fx.shareName)
	if enabled {
		t.Fatal("share enabled after post-verify fail — must stay disabled")
	}
}

func testRestoreInterruptedReset(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{useFailableResetable: true})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"i1.bin", "i2.bin"})
	all := fx.allHashes()
	fx.seedRemoteAll(all)

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot source: %v", werr)
	}

	// Mutate the store so the safety snap captures a recoverable
	// intermediate state. We delete files[0] — the safety snap will
	// reflect "only files[1] survives" and recovery must put us back to
	// that state.
	if err := fx.meta.DeleteFile(ctx, files[0].handle); err != nil {
		t.Fatalf("delete files[0]: %v", err)
	}

	// Arm Reset failure (one-shot).
	fx.failable.setFailNextReset(true)

	err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrRestoreAborted) {
		t.Fatalf("RestoreSnapshot err = %v, want errors.Is(ErrRestoreAborted)", err)
	}

	// Safety snapshot must exist for rollback.
	safetyID := fx.safetySnapshotID(ctx, snapID)
	if safetyID == "" {
		t.Fatal("safety snapshot ID not found — recovery primitive lost")
	}
	fileExistsNonEmpty(t,
		(&models.Snapshot{ID: safetyID}).MetadataDumpPath(fx.localStoreDir),
		"safety snap dump retained on disk")

	// Share stays disabled.
	enabled, _ := fx.rt.sharesSvc.IsShareEnabled(fx.shareName)
	if enabled {
		t.Fatal("share enabled after aborted restore — must stay disabled")
	}

	// --- Recovery: RestoreSnapshot(safetyID) ---
	// failNextReset already cleared (one-shot consumption). Underlying
	// MemoryMetadataStore.Reset will now run normally.
	if err := fx.rt.RestoreSnapshot(ctx, fx.shareName, safetyID, RestoreSnapshotOpts{}); err != nil {
		t.Fatalf("RestoreSnapshot(safetyID) recovery: %v", err)
	}

	// Post-recovery: files[1] is back (it was alive when the safety snap
	// was taken), files[0] is NOT (it was deleted before safety snap).
	if _, gerr := fx.meta.GetFile(ctx, files[1].handle); gerr != nil {
		t.Fatalf("files[1] missing after recovery: %v", gerr)
	}
	if _, gerr := fx.meta.GetFile(ctx, files[0].handle); gerr == nil {
		t.Fatal("files[0] present after recovery — safety snap captured the deleted state")
	}

	// Share STILL disabled after recovery.
	enabled, _ = fx.rt.sharesSvc.IsShareEnabled(fx.shareName)
	if enabled {
		t.Fatal("share enabled after recovery — must stay disabled")
	}
}

// ----- Fixture -----

// restoreFixture composes the moving parts the RestoreSnapshot integration
// suite needs. It is structurally aligned with the Phase 23 fixture but
// uses the REAL MemoryMetadataStore (Backup/Restore/Reset all natively
// supported) so populate -> snapshot -> mutate -> restore round-trips real
// data rather than the controlled-payload synthetic envelope.
type restoreFixture struct {
	t             *testing.T
	rt            *Runtime
	store         cpstore.Store
	meta          *metadatamemory.MemoryMetadataStore
	failable      *failableResetable // non-nil only when opts.useFailableResetable
	remote        *restoreRemote
	bs            *engine.BlockStore
	localStoreDir string
	shareName     string
	rootHandle    metadata.FileHandle

	mu       sync.Mutex
	files    []fileFixture
	hashSeen map[blockstore.ContentHash]struct{}
	hashList []blockstore.ContentHash
}

type fileFixture struct {
	name   string
	handle metadata.FileHandle
	hashes []blockstore.ContentHash
}

type restoreFixtureOpts struct {
	// shareEnabled controls the Enabled flag on the injected share. Default
	// (false) is the disabled state RestoreSnapshot requires; true exercises
	// the ErrShareEnabled precheck.
	shareEnabled bool

	// useFailableResetable wraps the metadata store in failableResetable so
	// the InterruptedRestore sub-test can flip failNextReset to simulate a
	// Reset failure mid-orchestration. Other sub-tests use the plain
	// MemoryMetadataStore directly.
	useFailableResetable bool
}

func newRestoreFixture(t *testing.T, opts restoreFixtureOpts) *restoreFixture {
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
	metaStoreName := "memory-restore"
	var registered metadata.MetadataStore = mem
	var failable *failableResetable
	if opts.useFailableResetable {
		failable = &failableResetable{MemoryMetadataStore: mem}
		registered = failable
	}
	if err := rt.RegisterMetadataStore(metaStoreName, registered); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	localStoreDir := t.TempDir()
	shareName := "restore-data"

	localStore := bsmemory.New()
	innerRemote := remotememory.New()
	t.Cleanup(func() { _ = innerRemote.Close() })
	wrappedRemote := newRestoreRemote(innerRemote)
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
		MetadataStore: metaStoreName,
		BlockStore:    bs,
		Enabled:       opts.shareEnabled,
	})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	// Bootstrap the share inside the metadata store so the file-create
	// helpers below have a valid root to attach files to.
	bgCtx := context.Background()
	if err := mem.CreateShare(bgCtx, &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("metadata.CreateShare: %v", err)
	}
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}
	rootFile, err := mem.CreateRootDirectory(bgCtx, shareName, rootAttr)
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle root: %v", err)
	}

	return &restoreFixture{
		t:             t,
		rt:            rt,
		store:         cp,
		meta:          mem,
		failable:      failable,
		remote:        wrappedRemote,
		bs:            bs,
		localStoreDir: localStoreDir,
		shareName:     shareName,
		rootHandle:    rootHandle,
		hashSeen:      make(map[blockstore.ContentHash]struct{}),
	}
}

func (f *restoreFixture) close() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.rt.Shutdown(ctx); err != nil {
		f.t.Logf("Shutdown: %v", err)
	}
}

func (f *restoreFixture) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	f.t.Cleanup(cancel)
	return ctx
}

// populateFiles creates one regular file per name with a unique BLAKE-seed
// block hash, wires it into the metadata store under the share root, and
// records the (handle, hash) pairs so the test can later mutate or assert.
// Each file gets exactly one unique block hash; allHashes() returns the
// dedup union.
func (f *restoreFixture) populateFiles(ctx context.Context, names []string) []fileFixture {
	f.t.Helper()
	out := make([]fileFixture, 0, len(names))
	for i, name := range names {
		handle, err := f.meta.GenerateHandle(ctx, f.shareName, "/"+name)
		if err != nil {
			f.t.Fatalf("GenerateHandle %q: %v", name, err)
		}
		_, fileID, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			f.t.Fatalf("DecodeFileHandle %q: %v", name, err)
		}
		hash := hashAllByte(byte(0x10 + i))
		file := &metadata.File{
			ID:        fileID,
			ShareName: f.shareName,
			FileAttr: metadata.FileAttr{
				Type:   metadata.FileTypeRegular,
				Mode:   0o644,
				UID:    1000,
				GID:    1000,
				Size:   4096,
				Blocks: []blockstore.BlockRef{{Hash: hash, Offset: 0, Size: 4096}},
			},
		}
		if err := f.meta.PutFile(ctx, file); err != nil {
			f.t.Fatalf("PutFile %q: %v", name, err)
		}
		if err := f.meta.SetParent(ctx, handle, f.rootHandle); err != nil {
			f.t.Fatalf("SetParent %q: %v", name, err)
		}
		if err := f.meta.SetChild(ctx, f.rootHandle, name, handle); err != nil {
			f.t.Fatalf("SetChild %q: %v", name, err)
		}
		if err := f.meta.SetLinkCount(ctx, handle, 1); err != nil {
			f.t.Fatalf("SetLinkCount %q: %v", name, err)
		}
		// Also seed the FileBlock side so EnumerateFileBlocks (post-verify
		// helper HashSetFromMetadataStore) returns the same hash union.
		fb := &metadata.FileBlock{
			ID:    fileID.String() + "-blk-0",
			Hash:  hash,
			State: blockstore.BlockStateRemote,
		}
		if err := f.meta.Put(ctx, fb); err != nil {
			f.t.Fatalf("Put FileBlock %q: %v", name, err)
		}

		f.mu.Lock()
		if _, seen := f.hashSeen[hash]; !seen {
			f.hashSeen[hash] = struct{}{}
			f.hashList = append(f.hashList, hash)
		}
		ff := fileFixture{name: name, handle: handle, hashes: []blockstore.ContentHash{hash}}
		f.files = append(f.files, ff)
		f.mu.Unlock()

		out = append(out, ff)
	}
	return out
}

func (f *restoreFixture) allHashes() []blockstore.ContentHash {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]blockstore.ContentHash, len(f.hashList))
	copy(out, f.hashList)
	return out
}

func (f *restoreFixture) seedRemoteAll(hashes []blockstore.ContentHash) {
	f.t.Helper()
	for _, h := range hashes {
		if err := f.remote.inner.Put(context.Background(), h, []byte("payload-"+h.String())); err != nil {
			f.t.Fatalf("seed remote put: %v", err)
		}
	}
}

func (f *restoreFixture) countFiles(ctx context.Context) int {
	f.t.Helper()
	// Walk via EnumerateFileBlocks as a cheap "is the store still populated"
	// proxy; the integration tests just need a before/after delta witness.
	n := 0
	_ = f.meta.EnumerateFileBlocks(ctx, func(blockstore.ContentHash) error {
		n++
		return nil
	})
	return n
}

// ----- restoreRemote: head-injection wrapper for failure-mode tests -----

// restoreRemote wraps a memory RemoteStore and exposes a phase-aware
// head-failure injection knob. Each Head call increments a global call
// counter; when failHashAfterCount(hash, n) is set, the (n+1)-th and
// subsequent Head calls for hash return ErrChunkNotFound until the knob is
// cleared. Pre-verify (called first in RestoreSnapshot) sees count==0; the
// safety-snap create + post-verify see higher counts, so a count of 1
// passes pre-verify and fails post-verify.
type restoreRemote struct {
	inner *remotememory.Store

	mu             sync.Mutex
	hashCallCounts map[blockstore.ContentHash]int
	failAfter      map[blockstore.ContentHash]int // hash -> threshold (calls < threshold pass; calls >= threshold fail)
}

func newRestoreRemote(inner *remotememory.Store) *restoreRemote {
	return &restoreRemote{
		inner:          inner,
		hashCallCounts: make(map[blockstore.ContentHash]int),
		failAfter:      make(map[blockstore.ContentHash]int),
	}
}

// failHashAfterCount configures Head() to return ErrChunkNotFound for hash
// on the (threshold+1)-th call counted from NOW (subsequent calls after
// gate-arm). The threshold is interpreted relative to the call counter
// observed at the moment of arming: a value of 0 means "fail the very
// next call and every subsequent one"; 1 means "the next call passes,
// the one after fails", etc. Set threshold negative to clear an active
// gate.
//
// Counting-from-now decouples test timing from incidental Head() calls
// the harness made before arming the gate (e.g. the source snapshot's
// own VerifyRemoteDurability ran Head() once per manifest hash during
// CreateSnapshot, well before the test reached RestoreSnapshot).
func (r *restoreRemote) failHashAfterCount(hash blockstore.ContentHash, threshold int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if threshold < 0 {
		delete(r.failAfter, hash)
		return
	}
	current := r.hashCallCounts[hash]
	r.failAfter[hash] = current + threshold
}

func (r *restoreRemote) Put(ctx context.Context, hash blockstore.ContentHash, data []byte) error {
	return r.inner.Put(ctx, hash, data)
}

func (r *restoreRemote) Get(ctx context.Context, hash blockstore.ContentHash) ([]byte, error) {
	return r.inner.Get(ctx, hash)
}

func (r *restoreRemote) GetRange(ctx context.Context, hash blockstore.ContentHash, offset, length int64) ([]byte, error) {
	return r.inner.GetRange(ctx, hash, offset, length)
}

func (r *restoreRemote) Delete(ctx context.Context, hash blockstore.ContentHash) error {
	return r.inner.Delete(ctx, hash)
}

func (r *restoreRemote) Head(ctx context.Context, hash blockstore.ContentHash) (blockstore.Meta, error) {
	r.mu.Lock()
	count := r.hashCallCounts[hash]
	r.hashCallCounts[hash] = count + 1
	threshold, hasGate := r.failAfter[hash]
	r.mu.Unlock()
	if hasGate && count >= threshold {
		return blockstore.Meta{}, blockstore.ErrChunkNotFound
	}
	return r.inner.Head(ctx, hash)
}

func (r *restoreRemote) Walk(ctx context.Context, fn func(hash blockstore.ContentHash, meta blockstore.Meta) error) error {
	return r.inner.Walk(ctx, fn)
}

func (r *restoreRemote) ReadBlockVerified(ctx context.Context, hash, expected blockstore.ContentHash) ([]byte, error) {
	return r.inner.ReadBlockVerified(ctx, hash, expected)
}

func (r *restoreRemote) Close() error { return r.inner.Close() }

func (r *restoreRemote) HealthCheck(ctx context.Context) error { return r.inner.HealthCheck(ctx) }

func (r *restoreRemote) Healthcheck(ctx context.Context) health.Report {
	return r.inner.Healthcheck(ctx)
}

// Compile-time guard: restoreRemote satisfies remote.RemoteStore.
var _ remote.RemoteStore = (*restoreRemote)(nil)

// ----- failableResetable: Reset-injection wrapper for InterruptedRestore -----

// failableResetable embeds a MemoryMetadataStore and forwards every
// MetadataStore + Backupable method via method promotion. The Reset method
// is overridden: when failNextReset is set the call returns a synthetic
// error and the flag is consumed (one-shot). All other methods delegate
// unchanged.
//
// The wrapper lives only inside this _test.go file under `package runtime`
// — it is not reachable from production code, which keeps T-24-04-03 in
// the threat model honest.
type failableResetable struct {
	*metadatamemory.MemoryMetadataStore

	mu             sync.Mutex
	failNextReset  bool
	resetCallCount int
}

func (f *failableResetable) Reset(ctx context.Context) error {
	f.mu.Lock()
	shouldFail := f.failNextReset
	if shouldFail {
		f.failNextReset = false
	}
	f.resetCallCount++
	f.mu.Unlock()
	if shouldFail {
		return errors.New("synthetic reset failure injected by test")
	}
	return f.MemoryMetadataStore.Reset(ctx)
}

func (f *failableResetable) setFailNextReset(v bool) {
	f.mu.Lock()
	f.failNextReset = v
	f.mu.Unlock()
}

// Compile-time guards: failableResetable satisfies both capabilities.
var _ metadata.Resetable = (*failableResetable)(nil)
var _ metadata.Backupable = (*failableResetable)(nil)

// ----- shared helpers (subset duplicated from snapshot_integration_test.go) -----

// Note: hashAllByte and mustFileNonEmpty are defined in
// snapshot_integration_test.go (same package). We re-use them directly.

// safetySnapshotID returns the ID of the most-recently-created snapshot
// for shareName that is NOT sourceID. Used by InterruptedRestore +
// PostVerifyFails to discover the safety snap created mid-restore.
func (f *restoreFixture) safetySnapshotID(ctx context.Context, sourceID string) string {
	f.t.Helper()
	snaps, err := f.store.ListSnapshots(ctx, f.shareName)
	if err != nil {
		f.t.Fatalf("ListSnapshots: %v", err)
	}
	for _, s := range snaps {
		if s.ID != sourceID {
			return s.ID
		}
	}
	return ""
}

// fileExistsNonEmpty is a small assertion helper used in the destructive-path
// scenarios.
func fileExistsNonEmpty(t *testing.T, path, label string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: stat %q: %v", label, path, err)
	}
	if st.Size() == 0 {
		t.Fatalf("%s: %q is empty, want non-empty", label, path)
	}
}
