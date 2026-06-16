package runtime

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// byteVerifyFixture wires a Runtime over the REAL production write path: a
// real local-fs CAS block store created via rt.AddShare (so the engine gets
// its metadata coordinator + rollup worker), backed by a pluggable metadata
// store. Unlike the inject-FileAttr.Blocks fixtures elsewhere in this
// package, every byte here flows engine.WriteAt -> rollup chunker -> CAS, and
// FileAttr.Blocks is persisted by the post-flush coordinator, not by the test.
type byteVerifyFixture struct {
	t             *testing.T
	rt            *Runtime
	store         cpstore.Store
	meta          metadata.Store
	bs            *engine.Store
	shareName     string
	localStoreDir string

	// Captured so simulateRestart can rebuild the Runtime over the SAME
	// control-plane store and re-register the reopened metadata store.
	metaStoreName string
	localID       string
	remoteID      string
}

// newByteVerifyFixture builds the fixture for the given metadata store. The
// metaType is the engine label recorded in the cpstore ("memory" | "badger" |
// "postgres") — it drives snapshot/restore's per-engine Restoreable dispatch.
func newByteVerifyFixture(t *testing.T, meta metadata.Store, metaType string) *byteVerifyFixture {
	return newByteVerifyFixtureOpts(t, meta, metaType, nil)
}

// newByteVerifyFixtureOpts is newByteVerifyFixture with an optional remote
// block store. When remoteCfg is non-nil it is persisted as a remote
// BlockStoreConfig and wired onto the share (RemoteBlockStoreID), so the
// engine gets a real syncer + remote target and snapshot create runs the
// full drain -> VerifyRemoteDurability gate. nil keeps the share local-only.
func newByteVerifyFixtureOpts(t *testing.T, meta metadata.Store, metaType string, remoteCfg *models.BlockStoreConfig) *byteVerifyFixture {
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
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{MaxSize: 0})

	const metaStoreName = "bv-meta"
	metaID, err := cp.CreateMetadataStore(context.Background(), &models.MetadataStoreConfig{
		Name: metaStoreName,
		Type: metaType,
	})
	if err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore(metaStoreName, meta); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	// Real local-fs CAS block store. The "fs" type drives the mandatory
	// append-log + rollup worker; chunks land in <dir>/shares/<share>/blocks/
	// and survive ResetLocalState (which only clears the append-log).
	fsDir := t.TempDir()
	localCfg := &models.BlockStoreConfig{
		Name: "bv-local",
		Kind: models.BlockStoreKindLocal,
		Type: "fs",
	}
	// SetConfig serializes into the persisted JSON blob; GetBlockStoreByID
	// reloads from the DB where only that blob survives (ParsedConfig is
	// gorm:"-" and is dropped on the round-trip).
	if err := localCfg.SetConfig(map[string]any{"path": fsDir}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	localID, err := cp.CreateBlockStore(context.Background(), localCfg)
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	// Optional remote block store: persist it and capture its ID so the
	// share is wired with a real syncer + remote target.
	var remoteID string
	if remoteCfg != nil {
		rid, rerr := cp.CreateBlockStore(context.Background(), remoteCfg)
		if rerr != nil {
			t.Fatalf("CreateBlockStore(remote): %v", rerr)
		}
		remoteID = rid
	}

	shareName := "/bv-share"
	// AddShare creates the root directory but not the Share record itself;
	// the metadata store needs the Share row so GetRootHandle can resolve it.
	if err := meta.CreateShare(context.Background(), &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("metadata CreateShare: %v", err)
	}
	// Persist the cpstore Share row so DisableShare (which writes
	// shares.enabled in the DB) resolves it. AddShare only populates the
	// runtime registry, not this row.
	cpShare := &models.Share{
		Name:              shareName,
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID,
		Enabled:           true,
	}
	if remoteID != "" {
		cpShare.RemoteBlockStoreID = &remoteID
	}
	if _, err := cp.CreateShare(context.Background(), cpShare); err != nil {
		t.Fatalf("cpstore CreateShare: %v", err)
	}
	if err := rt.AddShare(context.Background(), &shares.ShareConfig{
		Name:               shareName,
		MetadataStore:      metaStoreName,
		LocalBlockStoreID:  localID,
		RemoteBlockStoreID: remoteID,
		Enabled:            true,
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	t.Cleanup(func() { _ = rt.RemoveShare(shareName) })

	share, err := rt.GetShare(shareName)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if share.BlockStore == nil {
		t.Fatal("share.BlockStore nil after AddShare with fs local store")
	}
	localStoreDir, err := rt.sharesSvc.LocalStoreDir(shareName)
	if err != nil || localStoreDir == "" {
		t.Fatalf("LocalStoreDir: dir=%q err=%v (snapshot needs an on-disk root)", localStoreDir, err)
	}

	return &byteVerifyFixture{
		t:             t,
		rt:            rt,
		store:         cp,
		meta:          meta,
		bs:            share.BlockStore,
		shareName:     shareName,
		localStoreDir: localStoreDir,
		metaStoreName: metaStoreName,
		localID:       localID,
		remoteID:      remoteID,
	}
}

// simulateRestart models a process restart for an on-disk metadata backend:
// it shuts the current Runtime down, closes the old metadata store, reopens it
// from its durable location via reopen(), and builds a FRESH Runtime over the
// SAME control-plane store (where the restore marker lives) with the share
// re-added. Used by the crash-recovery test to prove startup recovery rolls
// back after a genuine reopen — not just an in-memory re-register. The cp
// store is intentionally NOT closed (it is the durable marker home).
func (f *byteVerifyFixture) simulateRestart(reopen func(*testing.T) metadata.Store) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = f.rt.Shutdown(ctx)
	cancel()

	// Close the old store before reopening: an on-disk backend (badger) holds
	// an exclusive lock on its directory, so the reopen would fail until the
	// prior handle is released. The first open()'s t.Cleanup will later call
	// Close() again on this now-closed handle at test teardown — harmless: the
	// error is ignored here and there, and neither badger nor postgres
	// corrupts state on a double Close.
	_ = f.meta.Close()

	meta := reopen(f.t)

	rt := New(f.store)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{MaxSize: 0})
	if err := rt.RegisterMetadataStore(f.metaStoreName, meta); err != nil {
		f.t.Fatalf("simulateRestart RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(context.Background(), &shares.ShareConfig{
		Name:               f.shareName,
		MetadataStore:      f.metaStoreName,
		LocalBlockStoreID:  f.localID,
		RemoteBlockStoreID: f.remoteID,
		Enabled:            false, // restore requires the share disabled
	}); err != nil {
		f.t.Fatalf("simulateRestart AddShare: %v", err)
	}
	share, err := rt.GetShare(f.shareName)
	if err != nil {
		f.t.Fatalf("simulateRestart GetShare: %v", err)
	}
	f.rt = rt
	f.meta = meta
	f.bs = share.BlockStore
}

func (f *byteVerifyFixture) close() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.rt.Shutdown(ctx); err != nil {
		f.t.Logf("Shutdown: %v", err)
	}
}

// createEmptyFile creates a regular file inode under the share root. It sets a
// deterministic path-based PayloadID via metadata.BuildPayloadID purely for
// test self-consistency (the fixture both stores and queries the same value);
// the production create path now derives a UUID-based PayloadID (#1166 PR-3).
// It does NOT set FileAttr.Blocks — the engine's post-flush coordinator owns that.
func (f *byteVerifyFixture) createEmptyFile(ctx context.Context, name string) metadata.PayloadID {
	f.t.Helper()
	root, err := f.meta.GetRootHandle(ctx, f.shareName)
	if err != nil {
		f.t.Fatalf("GetRootHandle: %v", err)
	}
	path := "/" + name
	handle, err := f.meta.GenerateHandle(ctx, f.shareName, path)
	if err != nil {
		f.t.Fatalf("GenerateHandle %q: %v", name, err)
	}
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		f.t.Fatalf("DecodeFileHandle %q: %v", name, err)
	}
	payloadID := metadata.PayloadID(metadata.BuildPayloadID(f.shareName, path))
	file := &metadata.File{
		ID:        fileID,
		ShareName: f.shareName,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			UID:       1000,
			GID:       1000,
			Size:      0,
			PayloadID: payloadID,
		},
	}
	if err := f.meta.PutFile(ctx, file); err != nil {
		f.t.Fatalf("PutFile %q: %v", name, err)
	}
	if err := f.meta.SetParent(ctx, handle, root); err != nil {
		f.t.Fatalf("SetParent %q: %v", name, err)
	}
	if err := f.meta.SetChild(ctx, root, name, handle); err != nil {
		f.t.Fatalf("SetChild %q: %v", name, err)
	}
	if err := f.meta.SetLinkCount(ctx, handle, 1); err != nil {
		f.t.Fatalf("SetLinkCount %q: %v", name, err)
	}
	return payloadID
}

// writeFile writes the full payload at offset 0 through the real engine path
// then flushes so the rollup chunker emits CAS blocks and the post-flush
// coordinator persists FileAttr.Blocks + ObjectID.
func (f *byteVerifyFixture) writeFile(ctx context.Context, payloadID metadata.PayloadID, data []byte) {
	f.t.Helper()
	if err := common.WriteToBlockStore(ctx, f.bs, payloadID, data, 0); err != nil {
		f.t.Fatalf("WriteToBlockStore %q: %v", payloadID, err)
	}
	if err := common.CommitBlockStore(ctx, f.bs, payloadID); err != nil {
		f.t.Fatalf("CommitBlockStore %q: %v", payloadID, err)
	}
	// Force the async rollup worker to roll the just-flushed payload into CAS
	// + the FileBlock manifest NOW, bypassing the stabilization window. This
	// is what the post-rollup coordinator hook uses to persist
	// FileAttr.Blocks + ObjectID — the snapshot-create orchestration drains
	// the same way before Backup(). Without it the bytes live only in the
	// append-log and FileAttr.Blocks stays empty.
	if err := f.bs.DrainRollups(ctx); err != nil {
		f.t.Fatalf("DrainRollups %q: %v", payloadID, err)
	}
}

// readFile reads count bytes at offset 0 back through the real engine path.
func (f *byteVerifyFixture) readFile(ctx context.Context, payloadID metadata.PayloadID, count int) []byte {
	f.t.Helper()
	res, err := common.ReadFromBlockStore(ctx, f.bs, payloadID, 0, uint32(count))
	if err != nil {
		f.t.Fatalf("ReadFromBlockStore %q: %v", payloadID, err)
	}
	out := make([]byte, len(res.Data))
	copy(out, res.Data)
	res.Release()
	return out
}

// getFile re-loads the file inode so callers see the persisted
// FileAttr.Blocks the post-flush coordinator wrote.
func (f *byteVerifyFixture) getFile(ctx context.Context, name string) *metadata.File {
	f.t.Helper()
	root, err := f.meta.GetRootHandle(ctx, f.shareName)
	if err != nil {
		f.t.Fatalf("GetRootHandle: %v", err)
	}
	handle, err := f.meta.GetChild(ctx, root, name)
	if err != nil {
		f.t.Fatalf("GetChild %q: %v", name, err)
	}
	file, err := f.meta.GetFile(ctx, handle)
	if err != nil {
		f.t.Fatalf("GetFile %q: %v", name, err)
	}
	return file
}

// fileExists reports whether `name` is still a child of the share root.
func (f *byteVerifyFixture) fileExists(ctx context.Context, name string) bool {
	f.t.Helper()
	root, err := f.meta.GetRootHandle(ctx, f.shareName)
	if err != nil {
		f.t.Fatalf("GetRootHandle: %v", err)
	}
	_, err = f.meta.GetChild(ctx, root, name)
	if err == nil {
		return true
	}
	if metadata.IsNotFoundError(err) {
		return false
	}
	f.t.Fatalf("GetChild %q: %v", name, err)
	return false
}

// deleteFile removes the file the production way: unlink the inode, then drive
// engine.Delete with the file's persisted FileAttr.Blocks so the coordinator
// decrements refcounts and the file_blocks rows are reclaimed (mirrors the
// NFS/SMB remove handlers). Passing the real Blocks avoids leaving orphaned
// file_blocks rows that would skew the next snapshot's manifest.
func (f *byteVerifyFixture) deleteFile(ctx context.Context, name string) {
	f.t.Helper()
	file := f.getFile(ctx, name)
	root, err := f.meta.GetRootHandle(ctx, f.shareName)
	if err != nil {
		f.t.Fatalf("GetRootHandle: %v", err)
	}
	handle, err := f.meta.GetChild(ctx, root, name)
	if err != nil {
		f.t.Fatalf("GetChild %q: %v", name, err)
	}
	if err := f.meta.DeleteFile(ctx, handle); err != nil {
		f.t.Fatalf("DeleteFile %q: %v", name, err)
	}
	if err := f.bs.Delete(ctx, string(file.PayloadID), file.Blocks); err != nil {
		f.t.Fatalf("bs.Delete %q: %v", name, err)
	}
}

// distinctBytes returns n bytes of non-repeating content seeded by `seed`, so
// FastCDC produces real content-defined chunk boundaries on multi-MiB inputs.
func distinctBytes(n int, seed uint64) []byte {
	out := make([]byte, n)
	x := seed*2654435761 + 0x9E3779B97F4A7C15
	for i := range out {
		// xorshift64* — cheap, non-repeating, content varies enough for CDC.
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27
		out[i] = byte((x * 0x2545F4914F6CDD1D) >> 56)
	}
	return out
}

// TestSnapshotByteVerify_MinimalProof is the de-risk step: prove a single
// small file round-trips byte-identical through the REAL write/flush/read path
// on the memory backend before layering on snapshot/restore.
func TestSnapshotByteVerify_MinimalProof(t *testing.T) {
	meta := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()

	ctx := context.Background()
	want := distinctBytes(4096, 1)
	pid := fx.createEmptyFile(ctx, "hello.bin")
	fx.writeFile(ctx, pid, want)

	got := fx.readFile(ctx, pid, len(want))
	if !bytes.Equal(got, want) {
		t.Fatalf("minimal proof byte mismatch: %s", firstDiff(want, got))
	}
}

// runByteVerifyCycle drives the full snapshot -> mutate -> drop-cache ->
// restore cycle against one metadata backend and asserts byte-identical
// recovery. Every byte flows through the real engine write/flush/read path;
// FileAttr.Blocks is owned by the coordinator, never injected.
func runByteVerifyCycle(t *testing.T, fx *byteVerifyFixture) {
	ctx := context.Background()

	// (1) Create 3 files with distinct real bytes. fileA is multi-chunk
	// (3 MiB of non-repeating bytes) so FastCDC produces several CAS chunks.
	const mib = 1 << 20
	origA := distinctBytes(3*mib, 0xA)
	origB := distinctBytes(8192, 0xB)
	origC := distinctBytes(64*1024, 0xC) // created during mutation, asserted gone after restore

	pidA := fx.createEmptyFile(ctx, "fileA.bin")
	fx.writeFile(ctx, pidA, origA)
	pidB := fx.createEmptyFile(ctx, "fileB.bin")
	fx.writeFile(ctx, pidB, origB)

	// Sanity: both files read back byte-identical pre-snapshot (proves the
	// real write path before we even snapshot).
	if got := fx.readFile(ctx, pidA, len(origA)); !bytes.Equal(got, origA) {
		t.Fatalf("pre-snapshot fileA mismatch: %s", firstDiff(origA, got))
	}
	if got := fx.readFile(ctx, pidB, len(origB)); !bytes.Equal(got, origB) {
		t.Fatalf("pre-snapshot fileB mismatch: %s", firstDiff(origB, got))
	}
	// Confirm fileA really is multi-chunk through the engine (FastCDC split).
	if fa := fx.getFile(ctx, "fileA.bin"); len(fa.Blocks) < 2 {
		t.Fatalf("fileA produced %d CAS block(s), want >= 2 (multi-chunk path not exercised)", len(fa.Blocks))
	}

	// (2) Snapshot the populated state. Local-only share -> NoVerify.
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snap.State = %q, want ready", snap.State)
	}

	// (3) Mutate. (a) overwrite fileA in place with different bytes of the
	// SAME length (the C2 in-place-overwrite rollback case), (b) delete
	// fileB, (c) create a new fileC.
	mutA := distinctBytes(3*mib, 0xA11) // same length, different content
	if len(mutA) != len(origA) {
		t.Fatalf("test bug: mutA len %d != origA len %d", len(mutA), len(origA))
	}
	fx.writeFile(ctx, pidA, mutA)
	if got := fx.readFile(ctx, pidA, len(mutA)); !bytes.Equal(got, mutA) {
		t.Fatalf("post-mutate fileA should read the NEW bytes pre-restore: %s", firstDiff(mutA, got))
	}
	fx.deleteFile(ctx, "fileB.bin")
	pidC := fx.createEmptyFile(ctx, "fileC.bin")
	fx.writeFile(ctx, pidC, origC)

	// (4) Drop local cache so reads must resolve through the restored CAS
	// manifest (the cold-state path that exposed #838 on postgres).
	if err := fx.bs.ResetLocalState(ctx); err != nil {
		t.Fatalf("ResetLocalState: %v", err)
	}

	// (5) Restore. Share must be disabled first; the snapshot is non-durable
	// (local-only) so AllowNonDurable is required.
	if err := fx.rt.DisableShare(ctx, fx.shareName); err != nil {
		t.Fatalf("DisableShare: %v", err)
	}
	if _, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{AllowNonDurable: true}); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	// (6) Re-read every file through the engine and assert byte-identical
	// recovery.
	//  - fileA: in-place overwrite rolled back -> original bytes (C2).
	//  - fileB: deleted post-snapshot -> recovered -> original bytes.
	//  - fileC: created post-snapshot -> gone.
	if !fx.fileExists(ctx, "fileA.bin") {
		t.Fatal("fileA missing after restore")
	}
	gotA := fx.readFile(ctx, pidA, len(origA))
	if !bytes.Equal(gotA, origA) {
		t.Fatalf("RESTORE fileA NOT byte-identical (in-place overwrite not rolled back / C2): %s", firstDiff(origA, gotA))
	}

	if !fx.fileExists(ctx, "fileB.bin") {
		t.Fatal("fileB not recovered after restore (deleted file should be back)")
	}
	pidB2 := fx.getFile(ctx, "fileB.bin").PayloadID
	gotB := fx.readFile(ctx, pidB2, len(origB))
	if !bytes.Equal(gotB, origB) {
		t.Fatalf("RESTORE fileB NOT byte-identical: %s", firstDiff(origB, gotB))
	}

	if fx.fileExists(ctx, "fileC.bin") {
		t.Fatal("fileC still present after restore (created post-snapshot, should be gone)")
	}

	// Share stays disabled after restore.
	if enabled, _ := fx.rt.sharesSvc.IsShareEnabled(fx.shareName); enabled {
		t.Fatal("share enabled after restore — must stay disabled")
	}
}

// TestSnapshotByteVerify_Matrix is the table-driven matrix over metadata-store
// backends. memory + badger always run; postgres runs only when
// DITTOFS_TEST_POSTGRES_DSN is set (the integration-tagged file supplies the
// postgres case constructor).
func TestSnapshotByteVerify_Matrix(t *testing.T) {
	for _, bk := range byteVerifyBackends(t) {
		bk := bk
		t.Run(bk.name, func(t *testing.T) {
			if bk.skip != "" {
				t.Skip(bk.skip)
			}
			meta, metaType := bk.open(t)
			fx := newByteVerifyFixture(t, meta, metaType)
			defer fx.close()
			runByteVerifyCycle(t, fx)
		})
	}
}

// byteVerifyBackend describes one metadata-store backend in the matrix.
type byteVerifyBackend struct {
	name string
	// open constructs the metadata store and returns it + its engine label.
	open func(t *testing.T) (metadata.Store, string)
	// reopen re-opens the SAME durable store (badger dir / postgres DSN)
	// after a simulated restart. nil for backends that cannot survive a
	// restart (memory) — the crash-recovery-reopen test skips those.
	reopen func(t *testing.T) metadata.Store
	// skip, when non-empty, marks the case as skipped with this reason.
	skip string
}

// postgresByteVerifyBackend is installed by the integration-tagged
// companion file's init(). Under plain `go test` it stays nil and the
// postgres matrix row is skipped with a build-tag hint.
var postgresByteVerifyBackend *byteVerifyBackend

// byteVerifyBackends returns the backend matrix: memory + badger always,
// postgres only when the integration-tagged companion installed it (and the
// DSN env is set, checked inside its open()).
func byteVerifyBackends(t *testing.T) []byteVerifyBackend {
	t.Helper()
	backends := []byteVerifyBackend{
		{
			name: "memory",
			open: func(t *testing.T) (metadata.Store, string) {
				return metadatamemory.NewMemoryMetadataStoreWithDefaults(), "memory"
			},
		},
		newBadgerByteVerifyBackend(t),
	}
	if postgresByteVerifyBackend != nil {
		backends = append(backends, *postgresByteVerifyBackend)
	} else {
		backends = append(backends, byteVerifyBackend{
			name: "postgres",
			skip: "postgres case requires -tags=integration and DITTOFS_TEST_POSTGRES_DSN",
		})
	}
	return backends
}

// newBadgerByteVerifyBackend builds the badger matrix entry with reopen
// support: open and reopen both target the SAME captured directory, so a
// simulateRestart reopens the persisted KV state (badger replays its WAL),
// modeling a real process restart rather than an in-memory re-register.
func newBadgerByteVerifyBackend(t *testing.T) byteVerifyBackend {
	t.Helper()
	dir := t.TempDir()
	openAt := func(t *testing.T) metadata.Store {
		store, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), dir)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	return byteVerifyBackend{
		name:   "badger",
		open:   func(t *testing.T) (metadata.Store, string) { return openAt(t), "badger" },
		reopen: openAt,
	}
}

// firstDiff returns a human-readable description of the first differing byte
// offset between want and got (or a length-mismatch note).
func firstDiff(want, got []byte) string {
	if len(want) != len(got) {
		n := len(want)
		if len(got) < n {
			n = len(got)
		}
		for i := 0; i < n; i++ {
			if want[i] != got[i] {
				return fmt.Sprintf("len want=%d got=%d; first byte diff at offset %d (want 0x%02x got 0x%02x)",
					len(want), len(got), i, want[i], got[i])
			}
		}
		return fmt.Sprintf("length mismatch: want=%d got=%d (shorter is a prefix of longer)", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Sprintf("first byte diff at offset %d (want 0x%02x got 0x%02x)", i, want[i], got[i])
		}
	}
	return "no difference"
}
