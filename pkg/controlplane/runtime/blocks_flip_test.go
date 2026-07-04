package runtime

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	sqlitemeta "github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// createFileForPayload creates a regular-file inode under the share root in the
// share's metadata store and returns its PayloadID — the key the block store's
// write/rollup path is driven by. Mirrors the createEmptyFile helper used by
// the snapshot byte-verify fixture.
func createFileForPayload(t *testing.T, ctx context.Context, meta metadata.Store, shareName, name string) (metadata.PayloadID, metadata.FileHandle) {
	t.Helper()
	root, err := meta.GetRootHandle(ctx, shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	path := "/" + name
	handle, err := meta.GenerateHandle(ctx, shareName, path)
	if err != nil {
		t.Fatalf("GenerateHandle %q: %v", name, err)
	}
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle %q: %v", name, err)
	}
	payloadID := metadata.PayloadID(metadata.BuildPayloadID(shareName, path))
	file := &metadata.File{
		ID:        fileID,
		ShareName: shareName,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			UID:       1000,
			GID:       1000,
			PayloadID: payloadID,
		},
	}
	if err := meta.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile %q: %v", name, err)
	}
	if err := meta.SetParent(ctx, handle, root); err != nil {
		t.Fatalf("SetParent %q: %v", name, err)
	}
	if err := meta.SetChild(ctx, root, name, handle); err != nil {
		t.Fatalf("SetChild %q: %v", name, err)
	}
	if err := meta.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount %q: %v", name, err)
	}
	return payloadID, handle
}

// flipTestCapabilities mirrors the sqlite conformance capabilities so a
// real sqlite metadata backend can be constructed in-process.
func flipTestCapabilities() metadata.FilesystemCapabilities {
	return metadata.FilesystemCapabilities{
		MaxReadSize:         1048576,
		PreferredReadSize:   1048576,
		MaxWriteSize:        1048576,
		PreferredWriteSize:  1048576,
		MaxFileSize:         9223372036854775807,
		MaxFilenameLen:      255,
		MaxPathLen:          4096,
		MaxHardLinkCount:    32767,
		SupportsHardLinks:   true,
		SupportsSymlinks:    true,
		CaseSensitive:       true,
		CasePreserving:      true,
		TimestampResolution: 1,
	}
}

// registerSQLiteMeta builds a real (on-disk) sqlite metadata store, registers
// it in the control-plane DB and the runtime, and returns the store handle so
// the test can assert LocalChunkIndex state directly. A real sqlite backend
// (not memory) exercises the T2 carryover: GetLocalLocation ↔ logblob ReadAt
// must agree on the production index backend once prod is flipped.
func registerSQLiteMeta(t *testing.T, rt *Runtime, cp cpstore.Store, name string) metadata.Store {
	t.Helper()
	ctx := context.Background()
	if _, err := cp.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: name, Type: "sqlite"}); err != nil {
		t.Fatalf("CreateMetadataStore(%s): %v", name, err)
	}
	dbPath := filepath.Join(t.TempDir(), name+".db")
	mds, err := sqlitemeta.NewSQLiteMetadataStore(ctx, &sqlitemeta.SQLiteMetadataStoreConfig{
		Path:        dbPath,
		AutoMigrate: true,
	}, flipTestCapabilities())
	if err != nil {
		t.Fatalf("NewSQLiteMetadataStore(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = mds.Close() })
	if err := rt.RegisterMetadataStore(name, mds); err != nil {
		t.Fatalf("RegisterMetadataStore(%s): %v", name, err)
	}
	return mds
}

// createFSLocalBlockStore creates an fs (log-blob) local block store config in
// the DB with a fresh temp path and returns its config ID. The fs backend is
// what exposes the log-blob substrate the carver reads from — a memory local
// store leaves carve disabled.
func createFSLocalBlockStore(t *testing.T, cp cpstore.Store, name string) string {
	t.Helper()
	cfg := &models.BlockStoreConfig{
		Name: name,
		Kind: models.BlockStoreKindLocal,
		Type: "fs",
	}
	if err := cfg.SetConfig(map[string]any{"path": t.TempDir()}); err != nil {
		t.Fatalf("SetConfig(%s): %v", name, err)
	}
	id, err := cp.CreateBlockStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CreateBlockStore(local %s): %v", name, err)
	}
	return id
}

// remoteForShare returns the underlying remote store for a share's remote
// config so the test can count CAS objects (Walk) vs block objects
// (WalkBlocks) directly on the memory backend.
func remoteForShare(t *testing.T, rt *Runtime) (remote.RemoteStore, remote.RemoteBlockStore) {
	t.Helper()
	entries := rt.sharesSvc.DistinctRemoteStores()
	if len(entries) == 0 {
		t.Fatal("no distinct remote stores registered")
	}
	rs := entries[0].Store
	rbs, ok := rs.(remote.RemoteBlockStore)
	if !ok {
		t.Fatalf("remote store %T does not implement RemoteBlockStore", rs)
	}
	return rs, rbs
}

func countCAS(t *testing.T, rs remote.RemoteStore) int {
	t.Helper()
	n := 0
	if err := rs.WalkLegacyChunks(context.Background(), func(block.ContentHash, int64) error { n++; return nil }); err != nil {
		t.Fatalf("WalkLegacyChunks (cas count): %v", err)
	}
	return n
}

func countBlocks(t *testing.T, rbs remote.RemoteBlockStore) int {
	t.Helper()
	n := 0
	if err := rbs.WalkBlocks(context.Background(), func(string, block.Meta) error { n++; return nil }); err != nil {
		t.Fatalf("WalkBlocks (block count): %v", err)
	}
	return n
}

// TestBlocksFlip_NewWriteCarvesToBlocks is the T6 activation proof at the
// runtime/shares level on a real (sqlite) metadata backend. Once the carve
// substrate is wired globally in createBlockStoreForShare, a fresh share's new
// write must produce a blocks/<id> object (carve) and NEVER a cas/<hash> object
// (legacy mirrorChunk). The read path resolves the block byte-identically, and
// the sqlite LocalChunkIndex round-trips (the T2 GetLocalLocation↔ReadAt
// carryover, previously exercised only on the memory index).
func TestBlocksFlip_NewWriteCarvesToBlocks(t *testing.T) {
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

	metaStore := registerSQLiteMeta(t, rt, cp, "sqlite-meta")
	localID := createFSLocalBlockStore(t, cp, "fs-local")
	// Registered AFTER createFSLocalBlockStore so t.Cleanup's LIFO order runs
	// this (which Close()s the share's block store, releasing the log-blob fd)
	// BEFORE the block store's t.TempDir() RemoveAll. On Windows an open handle
	// blocks unlink of blobs/*.blob; Unix tolerates unlink-while-open.
	t.Cleanup(func() {
		for _, name := range rt.ListShares() {
			_ = rt.RemoveShare(name)
		}
	})

	remoteCfg := &models.BlockStoreConfig{Name: "mem-remote", Kind: models.BlockStoreKindRemote, Type: "memory"}
	remoteID, err := cp.CreateBlockStore(ctx, remoteCfg)
	if err != nil {
		t.Fatalf("CreateBlockStore(remote): %v", err)
	}

	shareName := "/blocks-flip"
	if err := rt.AddShare(ctx, &ShareConfig{
		Name:               shareName,
		MetadataStore:      "sqlite-meta",
		LocalBlockStoreID:  localID,
		RemoteBlockStoreID: remoteID,
		Enabled:            true,
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	bs, err := rt.sharesSvc.GetBlockStoreForShare(shareName)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}

	payload, _ := createFileForPayload(t, ctx, metaStore, shareName, "file-1")
	data := bytes.Repeat([]byte("dittofs-blocks-flip-payload!"), 4096) // ~112 KiB
	if err := common.WriteToBlockStore(ctx, bs, payload, data, 0); err != nil {
		t.Fatalf("WriteToBlockStore: %v", err)
	}
	if err := common.CommitBlockStore(ctx, bs, payload); err != nil {
		t.Fatalf("CommitBlockStore: %v", err)
	}
	// Force rollup (append-log → log-blob + LocalChunkIndex + FileChunks) then
	// carve/mirror. On a flipped share this drains the carve set into blocks/.
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}

	rs, rbs := remoteForShare(t, rt)

	if got := countBlocks(t, rbs); got == 0 {
		t.Fatalf("no block object created — carve did not run (flip not wired); blocks=%d", got)
	}
	// Definitive proof mirrorChunk was never called on the new write path: its
	// ONLY observable output is a standalone cas/<hash> PUT (remoteStore.Put).
	// Zero cas objects with a block present means every chunk routed through the
	// carver, never the legacy standalone mirror. (SyncCounts is NOT a spy here:
	// the carve commit path shares the completedSyncs counter with mirrorChunk.)
	if got := countCAS(t, rs); got != 0 {
		t.Fatalf("new write produced %d cas/ object(s) — mirrorChunk ran (flip not wired)", got)
	}

	// T2 carryover on sqlite: every carved chunk keeps a LocalChunkIndex entry
	// (carve commits the local location; only GC removes it). Pick the payload's
	// chunk hashes and assert GetLocalLocation resolves — the same lookup the
	// index-hit read path uses (GetLocalLocation → ReadLocalAt).
	rows, err := metaStore.ListFileChunks(ctx, string(payload))
	if err != nil {
		t.Fatalf("ListFileChunks: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no FileChunk rows persisted for payload")
	}
	for _, fb := range rows {
		if fb == nil || fb.Hash.IsZero() {
			continue
		}
		loc, ok, err := metaStore.GetLocalLocation(ctx, fb.Hash)
		if err != nil {
			t.Fatalf("GetLocalLocation(%s): %v", fb.Hash, err)
		}
		if !ok {
			t.Fatalf("GetLocalLocation(%s): not found — carve did not persist local index on sqlite", fb.Hash)
		}
		if loc.RawLength == 0 {
			t.Fatalf("GetLocalLocation(%s): zero RawLength", fb.Hash)
		}
	}

	// Read back through the block path: byte-identical.
	res, err := common.ReadFromBlockStore(ctx, bs, payload, 0, uint32(len(data)))
	if err != nil {
		t.Fatalf("ReadFromBlockStore: %v", err)
	}
	if !bytes.Equal(res.Data, data) {
		t.Fatalf("read mismatch: got %d bytes not equal to written %d", len(res.Data), len(data))
	}
}

// writeAndCarve creates a file, writes data, and drains it into a carved block.
// Returns the payloadID and the blockID the chunk landed in.
func writeAndCarve(t *testing.T, ctx context.Context, bs *engine.Store, meta metadata.Store, shareName, name string, data []byte) (metadata.PayloadID, metadata.FileHandle, string) {
	t.Helper()
	payload, handle := createFileForPayload(t, ctx, meta, shareName, name)
	if err := common.WriteToBlockStore(ctx, bs, payload, data, 0); err != nil {
		t.Fatalf("WriteToBlockStore(%s): %v", name, err)
	}
	if err := common.CommitBlockStore(ctx, bs, payload); err != nil {
		t.Fatalf("CommitBlockStore(%s): %v", name, err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads(%s): %v", name, err)
	}
	rows, err := meta.ListFileChunks(ctx, string(payload))
	if err != nil || len(rows) == 0 {
		t.Fatalf("ListFileChunks(%s): rows=%d err=%v", name, len(rows), err)
	}
	loc, ok, err := meta.GetLocator(ctx, rows[0].Hash)
	if err != nil || !ok || loc.IsStandalone() {
		t.Fatalf("GetLocator(%s): ok=%v standalone=%v err=%v — chunk not carved into a block", name, ok, loc.IsStandalone(), err)
	}
	return payload, handle, loc.BlockID
}

// unlinkFile simulates a fully-reconciled unlink: it removes the file inode (so
// the file_block_refs / nlink>0 mark arm no longer references the hash) and
// drops every file_blocks (FileChunk) row for the payload (the unconditional
// mark arm). After both, EnumerateFileChunks no longer reports the hash — it is
// globally dead for the GC mark phase.
func unlinkFile(t *testing.T, ctx context.Context, meta metadata.Store, handle metadata.FileHandle, payload metadata.PayloadID) {
	t.Helper()
	rows, err := meta.ListFileChunks(ctx, string(payload))
	if err != nil {
		t.Fatalf("ListFileChunks(unlink): %v", err)
	}
	if err := meta.DeleteFile(ctx, handle); err != nil {
		t.Fatalf("DeleteFile(unlink): %v", err)
	}
	for _, fb := range rows {
		if err := meta.Delete(ctx, fb.ID); err != nil {
			t.Fatalf("Delete file chunk %q: %v", fb.ID, err)
		}
	}
}

// TestBlocksFlip_GCUnionReclaimerFreesOwnerOnly proves the runtime wires a
// per-remote UNION BlockReclaimer across every share on a shared (ref-counted)
// remote, and that a share which does NOT own a swept block is a clean no-op —
// no error, no spurious LiveChunkCount decrement (ADDITION 1).
//
// Shares A and B point at ONE remote config. Each carves its own block. After
// A's file is unlinked, a grace-0 GC sweep must free A's block (via A's
// metadata) while B's block record and its data are untouched.
func TestBlocksFlip_GCUnionReclaimerFreesOwnerOnly(t *testing.T) {
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

	metaA := registerSQLiteMeta(t, rt, cp, "meta-a")
	metaB := registerSQLiteMeta(t, rt, cp, "meta-b")
	localA := createFSLocalBlockStore(t, cp, "fs-a")
	localB := createFSLocalBlockStore(t, cp, "fs-b")
	// Registered AFTER both createFSLocalBlockStore calls so t.Cleanup's LIFO
	// order runs this (which Close()s each share's block store, releasing the
	// log-blob fds) BEFORE the block stores' t.TempDir() RemoveAll. On Windows
	// an open handle blocks unlink of blobs/*.blob; Unix tolerates unlink-open.
	t.Cleanup(func() {
		for _, name := range rt.ListShares() {
			_ = rt.RemoveShare(name)
		}
	})

	// ONE remote config, shared (ref-counted) by both shares.
	remoteID, err := cp.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "shared-remote", Kind: models.BlockStoreKindRemote, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore(remote): %v", err)
	}

	shareA, shareB := "/share-a", "/share-b"
	if err := rt.AddShare(ctx, &ShareConfig{Name: shareA, MetadataStore: "meta-a", LocalBlockStoreID: localA, RemoteBlockStoreID: remoteID, Enabled: true}); err != nil {
		t.Fatalf("AddShare A: %v", err)
	}
	if err := rt.AddShare(ctx, &ShareConfig{Name: shareB, MetadataStore: "meta-b", LocalBlockStoreID: localB, RemoteBlockStoreID: remoteID, Enabled: true}); err != nil {
		t.Fatalf("AddShare B: %v", err)
	}

	bsA, err := rt.sharesSvc.GetBlockStoreForShare(shareA)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare A: %v", err)
	}
	bsB, err := rt.sharesSvc.GetBlockStoreForShare(shareB)
	if err != nil {
		t.Fatalf("GetBlockStoreForShare B: %v", err)
	}

	// Distinct content per share → distinct blocks on the shared remote.
	dataA := bytes.Repeat([]byte("aaaa-share-A-content!"), 4096)
	dataB := bytes.Repeat([]byte("bbbb-share-B-content!"), 4096)
	payloadA, handleA, blockA := writeAndCarve(t, ctx, bsA, metaA, shareA, "a", dataA)
	_, _, blockB := writeAndCarve(t, ctx, bsB, metaB, shareB, "b", dataB)
	if blockA == blockB {
		t.Fatalf("blocks collided: A=%s B=%s (distinct content must yield distinct blocks)", blockA, blockB)
	}

	// One shared remote holds both blocks.
	_, rbs := remoteForShare(t, rt)
	if got := countBlocks(t, rbs); got != 2 {
		t.Fatalf("expected 2 block objects on shared remote, got %d", got)
	}

	// Capture B's block record (owner-untouched invariant).
	recBBefore, ok, err := metaB.GetBlockRecord(ctx, blockB)
	if err != nil || !ok {
		t.Fatalf("GetBlockRecord(B) before: ok=%v err=%v", ok, err)
	}

	// Unlink A's file: remove the inode + its FileChunk rows so the mark phase
	// sees A's hash as globally dead.
	unlinkFile(t, ctx, metaA, handleA, payloadA)

	// Grace-0 GC over the shared remote. This routes through the per-remote union
	// reclaimer spanning A and B.
	zero := time.Duration(0)
	if _, err := rt.runBlockGCForShare(ctx, shareA, false, nil, &zero); err != nil {
		t.Fatalf("runBlockGCForShare: %v", err)
	}

	// A's block freed remotely; only B's remains.
	if got := countBlocks(t, rbs); got != 1 {
		t.Fatalf("expected 1 block after GC (A freed), got %d", got)
	}
	// A's block record gone.
	if _, ok, err := metaA.GetBlockRecord(ctx, blockA); err != nil || ok {
		t.Fatalf("A block record should be deleted: ok=%v err=%v", ok, err)
	}
	// B's block record untouched: present with the SAME LiveChunkCount — B's
	// reclaimer was a clean no-op for A's dead hash.
	recBAfter, ok, err := metaB.GetBlockRecord(ctx, blockB)
	if err != nil || !ok {
		t.Fatalf("GetBlockRecord(B) after: ok=%v err=%v — B's record was wrongly touched", ok, err)
	}
	if recBAfter.LiveChunkCount != recBBefore.LiveChunkCount {
		t.Fatalf("B LiveChunkCount changed: before=%d after=%d — union reclaimer touched a non-owning share", recBBefore.LiveChunkCount, recBAfter.LiveChunkCount)
	}

	// B's data still reads byte-identical.
	res, err := common.ReadFromBlockStore(ctx, bsB, metadata.PayloadID(metadata.BuildPayloadID(shareB, "/b")), 0, uint32(len(dataB)))
	if err != nil {
		t.Fatalf("ReadFromBlockStore(B) after GC: %v", err)
	}
	if !bytes.Equal(res.Data, dataB) {
		t.Fatal("B data corrupted after A's GC")
	}
}
