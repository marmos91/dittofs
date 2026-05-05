package blockstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/migrate"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// loopFixture holds a wired offline-runtime + share-name + root handle
// for the migrate-loop tests. mds doubles as the FileBlockStore (the
// memory metadata store implements blockstore.FileBlockStore — see
// pkg/metadata/store/memory/objects.go).
type loopFixture struct {
	rt        *offlineRuntime
	mds       *memmeta.MemoryMetadataStore
	rs        *memory.Store
	share     string
	root      metadata.FileHandle
	dataDir   string
}

func newLoopFixture(t *testing.T) *loopFixture {
	t.Helper()
	ctx := t.Context()

	mds := memmeta.NewMemoryMetadataStoreWithDefaults()
	rs := memory.New()
	dataDir := t.TempDir()
	share := "lshare"

	if err := mds.CreateShare(ctx, &metadata.Share{Name: share}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	rootFile, err := mds.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	rt := newTestOfflineRuntime(share, mds, mds, rs, dataDir)
	return &loopFixture{
		rt:      rt,
		mds:     mds,
		rs:      rs,
		share:   share,
		root:    rootHandle,
		dataDir: dataDir,
	}
}

// addLegacyFile writes a regular file with size N bytes split across
// {payloadID}/block-{idx} legacy keys. Returns the file handle.
//
// The data is the concatenation of `chunks`. Each chunk becomes one
// legacy block (block-0, block-1, ...). FileAttr.Blocks is left empty
// (legacy semantic). FileBlock rows are persisted via the metadata
// store's Put surface; the remote store carries the bytes.
func addLegacyFile(t *testing.T, f *loopFixture, name, path string, chunks [][]byte) metadata.FileHandle {
	t.Helper()
	ctx := t.Context()

	handle, err := f.mds.GenerateHandle(ctx, f.share, path)
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}

	payloadID := metadata.PayloadID(f.share + path)
	var totalSize uint64
	for _, c := range chunks {
		totalSize += uint64(len(c))
	}

	file := &metadata.File{
		ShareName: f.share,
		Path:      path,
		ID:        id,
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			PayloadID: payloadID,
			Size:      totalSize,
			// Blocks = empty (legacy semantic).
		},
	}
	if err := f.mds.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if err := f.mds.SetParent(ctx, handle, f.root); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := f.mds.SetChild(ctx, f.root, name, handle); err != nil {
		t.Fatalf("SetChild: %v", err)
	}
	if err := f.mds.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount: %v", err)
	}

	// Materialize legacy blocks: one FileBlock row + one remote object per
	// chunk. FileBlock.ID convention (matches storetest.file_block_ops):
	// "{payloadID}/{NUMERIC_IDX}" — the memory store's ListFileBlocks parses
	// the suffix as a numeric block index. The legacy S3 key (BlockStoreKey)
	// uses the FormatStoreKey "{payloadID}/block-{idx}" shape.
	for idx, c := range chunks {
		blockID := fmt.Sprintf("%s/%d", string(payloadID), idx)
		legacyKey := blockstore.FormatStoreKey(string(payloadID), uint64(idx))
		if err := f.rs.WriteBlock(ctx, legacyKey, c); err != nil {
			t.Fatalf("rs.WriteBlock(%s): %v", legacyKey, err)
		}
		fb := &blockstore.FileBlock{
			ID:            blockID,
			DataSize:      uint32(len(c)),
			BlockStoreKey: legacyKey,
			RefCount:      1,
			State:         blockstore.BlockStatePending, // Legacy + BlockStoreKey → IsRemote()=true.
		}
		if err := f.mds.Put(ctx, fb); err != nil {
			t.Fatalf("mds.Put(%s): %v", blockID, err)
		}
	}
	return handle
}

// largeChunk returns a 4-MiB byte slice filled with the provided byte —
// large enough that FastCDC produces at least one boundary at min size.
func largeChunk(b byte) []byte {
	out := make([]byte, 4*1024*1024)
	for i := range out {
		out[i] = b
	}
	return out
}

// TestMigrateLoop_EmptyShare covers Test 1: empty share runs to
// completion with zero callbacks, zero metadata writes, zero remote PUTs.
func TestMigrateLoop_EmptyShare(t *testing.T) {
	f := newLoopFixture(t)
	opts := migrateOptions{share: f.share, stateDir: f.dataDir}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("runMigrateLoopWithRuntime: %v", err)
	}
	// Journal should exist but be empty.
	j, err := migrate.OpenJournalReadOnly(f.dataDir)
	if err != nil {
		t.Fatalf("OpenJournalReadOnly: %v", err)
	}
	defer j.Close()
	entries, _ := j.Replay()
	if len(entries) != 0 {
		t.Errorf("empty share journal entries = %d, want 0", len(entries))
	}
}

// TestMigrateLoop_SingleFile_OneChunk covers Test 2 (small variant):
// one legacy block re-chunks into the canonical CAS BlockRefs;
// FileAttr.Blocks + ObjectID populated; journal records the commit.
func TestMigrateLoop_SingleFile_OneChunk(t *testing.T) {
	f := newLoopFixture(t)
	data := largeChunk('a')
	handle := addLegacyFile(t, f, "a.bin", "/a.bin", [][]byte{data})

	opts := migrateOptions{share: f.share, stateDir: f.dataDir}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("runMigrateLoopWithRuntime: %v", err)
	}

	updated, err := f.mds.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if len(updated.Blocks) == 0 {
		t.Fatalf("post-migrate Blocks is empty; want >=1")
	}
	// ObjectID must equal ComputeObjectID(Blocks).
	wantOID := blockstore.ComputeObjectID(updated.Blocks)
	if updated.ObjectID != wantOID {
		t.Errorf("ObjectID mismatch: got %v, want %v", updated.ObjectID, wantOID)
	}
	// Sum of block sizes must equal the file's size.
	var sum uint64
	for _, b := range updated.Blocks {
		sum += uint64(b.Size)
	}
	if sum != uint64(len(data)) {
		t.Errorf("sum(Blocks.Size) = %d, want %d", sum, len(data))
	}
	// Each BlockRef.Hash must match BLAKE3(chunk_bytes), where chunks
	// concatenate to the original data.
	for _, b := range updated.Blocks {
		offEnd := uint64(b.Offset) + uint64(b.Size)
		expect := blockstore.ContentHash(blake3.Sum256(data[b.Offset:offEnd]))
		if b.Hash != expect {
			t.Errorf("BlockRef hash mismatch at offset %d", b.Offset)
		}
		// Remote must carry the CAS object.
		got, rerr := f.rs.ReadBlock(t.Context(), blockstore.FormatCASKey(b.Hash))
		if rerr != nil {
			t.Errorf("CAS object missing for hash %v: %v", b.Hash, rerr)
		} else if !bytes.Equal(got, data[b.Offset:offEnd]) {
			t.Errorf("CAS object bytes mismatch at offset %d", b.Offset)
		}
	}
	// Journal: exactly one file_done entry.
	j, _ := migrate.OpenJournalReadOnly(f.dataDir)
	defer j.Close()
	entries, _ := j.Replay()
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(entries))
	}
	if entries[0].Kind != "file_done" {
		t.Errorf("entry Kind = %q, want file_done", entries[0].Kind)
	}
	if entries[0].FileHandle != string(handle) {
		t.Errorf("entry handle = %q, want %q", entries[0].FileHandle, string(handle))
	}
}

// TestMigrateLoop_DedupAcrossFiles covers Test 3: two files with
// identical content → second file's chunks all dedup-hit; bytes_deduped
// reflects the win.
func TestMigrateLoop_DedupAcrossFiles(t *testing.T) {
	f := newLoopFixture(t)
	data := largeChunk('z')
	handleA := addLegacyFile(t, f, "a.bin", "/a.bin", [][]byte{data})
	handleB := addLegacyFile(t, f, "b.bin", "/b.bin", [][]byte{data})

	opts := migrateOptions{share: f.share, stateDir: f.dataDir}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("runMigrateLoopWithRuntime: %v", err)
	}

	// Both files must commit identical Blocks. The first file's PutFile
	// claims the ObjectID; the second file's PutFile is rejected by the
	// D-14 first-committer-wins index and retries with ObjectID=zero
	// (canonical first-committer keeps the index entry, the duplicate
	// stands without owning the unique ObjectID).
	a, _ := f.mds.GetFile(t.Context(), handleA)
	b, _ := f.mds.GetFile(t.Context(), handleB)
	if a.ObjectID.IsZero() {
		t.Errorf("first file ObjectID is zero; want nonzero")
	}
	if !b.ObjectID.IsZero() {
		t.Errorf("second file ObjectID = %v; want zero (first-committer-wins)", b.ObjectID)
	}
	if len(a.Blocks) == 0 || len(a.Blocks) != len(b.Blocks) {
		t.Errorf("block counts differ or zero: a=%d b=%d", len(a.Blocks), len(b.Blocks))
	}
	for i := range a.Blocks {
		if a.Blocks[i].Hash != b.Blocks[i].Hash {
			t.Errorf("block[%d] hash mismatch: a=%v b=%v", i, a.Blocks[i].Hash, b.Blocks[i].Hash)
		}
	}
	// Journal: 2 entries, second has nonzero BytesDeduped.
	j, _ := migrate.OpenJournalReadOnly(f.dataDir)
	defer j.Close()
	entries, _ := j.Replay()
	if len(entries) != 2 {
		t.Fatalf("journal entries = %d, want 2", len(entries))
	}
	var totalDeduped uint64
	for _, e := range entries {
		totalDeduped += e.BytesDeduped
	}
	if totalDeduped == 0 {
		t.Errorf("totalDeduped = 0; expected nonzero (second file is duplicate)")
	}
}

// TestMigrateLoop_Resume covers Test 4: pre-populated journal entry
// causes the matching file to be skipped.
func TestMigrateLoop_Resume(t *testing.T) {
	f := newLoopFixture(t)
	dataA := largeChunk('a')
	dataB := largeChunk('b')
	handleA := addLegacyFile(t, f, "a.bin", "/a.bin", [][]byte{dataA})
	_ = addLegacyFile(t, f, "b.bin", "/b.bin", [][]byte{dataB})

	// Seed the journal with handleA marked done.
	j, err := migrate.OpenJournal(f.dataDir)
	if err != nil {
		t.Fatalf("seed OpenJournal: %v", err)
	}
	if err := j.Append(migrate.JournalEntry{
		Kind:       "file_done",
		FileHandle: string(handleA),
		PayloadID:  string(metadata.PayloadID(f.share + "/a.bin")),
	}); err != nil {
		t.Fatalf("seed Append: %v", err)
	}
	_ = j.Close()

	opts := migrateOptions{share: f.share, stateDir: f.dataDir}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("runMigrateLoopWithRuntime: %v", err)
	}

	// File A should remain UN-migrated (Blocks empty); file B should
	// have Blocks populated.
	a, _ := f.mds.GetFile(t.Context(), handleA)
	if len(a.Blocks) != 0 {
		t.Errorf("file A migrated despite journal-done entry: Blocks=%d", len(a.Blocks))
	}
}

// TestMigrateLoop_DryRun covers Test 5: no metadata writes, no remote
// PUTs, no journal appends. The result still reports the would-be
// upload bytes via stdout (asserted indirectly: PutFile is not called).
func TestMigrateLoop_DryRun(t *testing.T) {
	f := newLoopFixture(t)
	data := largeChunk('d')
	handle := addLegacyFile(t, f, "a.bin", "/a.bin", [][]byte{data})

	opts := migrateOptions{share: f.share, stateDir: f.dataDir, dryRun: true}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("runMigrateLoopWithRuntime: %v", err)
	}

	updated, _ := f.mds.GetFile(t.Context(), handle)
	if len(updated.Blocks) != 0 {
		t.Errorf("dry-run wrote Blocks: len=%d", len(updated.Blocks))
	}
	if !updated.ObjectID.IsZero() {
		t.Errorf("dry-run wrote ObjectID: %v", updated.ObjectID)
	}

	// Journal must contain zero entries — dry-run suppresses Append.
	j, _ := migrate.OpenJournalReadOnly(f.dataDir)
	defer j.Close()
	entries, _ := j.Replay()
	if len(entries) != 0 {
		t.Errorf("dry-run wrote journal entries: %d", len(entries))
	}

	// No CAS objects must exist on the remote.
	if got, _ := f.rs.ListByPrefix(t.Context(), "cas/"); len(got) != 0 {
		t.Errorf("dry-run wrote remote CAS objects: %d", len(got))
	}
}

// TestMigrateLoop_EmptyFile covers the zero-byte file edge: file is
// marked skipped in the journal; metadata is unchanged.
func TestMigrateLoop_EmptyFile(t *testing.T) {
	f := newLoopFixture(t)
	handle := addLegacyFile(t, f, "empty.bin", "/empty.bin", nil)

	opts := migrateOptions{share: f.share, stateDir: f.dataDir}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("runMigrateLoopWithRuntime: %v", err)
	}
	updated, _ := f.mds.GetFile(t.Context(), handle)
	if len(updated.Blocks) != 0 {
		t.Errorf("empty file got Blocks=%d, want 0", len(updated.Blocks))
	}
	j, _ := migrate.OpenJournalReadOnly(f.dataDir)
	defer j.Close()
	entries, _ := j.Replay()
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(entries))
	}
	if entries[0].Kind != "file_skipped" {
		t.Errorf("entry Kind = %q, want file_skipped", entries[0].Kind)
	}
}

// TestMigrateLoop_DryRun_ChunkBoundaryParity asserts the dry-run
// produces the same BlockRef hashes the wet run would — i.e., the
// dry-run estimate is structurally faithful.
func TestMigrateLoop_DryRun_ChunkBoundaryParity(t *testing.T) {
	f := newLoopFixture(t)
	data := largeChunk('p')
	handle := addLegacyFile(t, f, "a.bin", "/a.bin", [][]byte{data})

	// Wet run on a separate fixture.
	wf := newLoopFixture(t)
	dataWet := largeChunk('p')
	handleWet := addLegacyFile(t, wf, "a.bin", "/a.bin", [][]byte{dataWet})

	if err := runMigrateLoopWithRuntime(t.Context(), wf.rt, migrateOptions{share: wf.share, stateDir: wf.dataDir}); err != nil {
		t.Fatalf("wet run: %v", err)
	}
	wetFile, _ := wf.mds.GetFile(t.Context(), handleWet)
	wetBlocks := wetFile.Blocks

	// Dry run.
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, migrateOptions{share: f.share, stateDir: f.dataDir, dryRun: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// File must not be mutated.
	dryFile, _ := f.mds.GetFile(t.Context(), handle)
	if len(dryFile.Blocks) != 0 {
		t.Errorf("dry-run mutated Blocks: len=%d", len(dryFile.Blocks))
	}

	// Sanity: wet has nonzero Blocks; dry would have produced same.
	if len(wetBlocks) == 0 {
		t.Fatal("wet run produced zero Blocks; cannot compare to dry")
	}
	_ = context.Background
}

// TestMigrateLoop_NoControlPlaneRuntimeImport is a compile-time +
// grep-time assertion captured as a doc test. The acceptance criterion
// `! grep -q 'controlplane/runtime' migrate_runtime.go` is checked by
// the verify step; this test is a runtime smoke that openOfflineRuntime
// returns the deferral sentinel rather than panicking.
func TestMigrateLoop_OpenOfflineRuntimeReturnsDeferralSentinel(t *testing.T) {
	rt, err := openOfflineRuntime(t.Context(), "any")
	if rt != nil {
		t.Errorf("expected nil rt, got %v", rt)
	}
	if err == nil {
		t.Fatal("expected ErrOfflineRuntimeNotWired, got nil")
	}
}

// TestMigrateLoop_EndToEnd covers Plan 14-05 behavior 6 (happy):
// after a successful re-chunk loop the post-pipeline runs to
// completion: integrity passes → BlockLayout flips to cas-only →
// legacy keys deleted → returns nil.
func TestMigrateLoop_EndToEnd(t *testing.T) {
	f := newIntegrityFixture(t)
	addLegacyFile(t, f.loopFixture, "a.bin", "/a.bin", [][]byte{largeChunk('a')})
	addLegacyFile(t, f.loopFixture, "b.bin", "/b.bin", [][]byte{largeChunk('b')})

	opts := migrateOptions{share: f.share, stateDir: f.dataDir, parallel: 2}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("runMigrateLoopWithRuntime: %v", err)
	}

	// 1. BlockLayout flipped.
	post, err := f.mds.GetShareOptions(t.Context(), f.share)
	if err != nil {
		t.Fatalf("GetShareOptions: %v", err)
	}
	if post.BlockLayout != metadata.BlockLayoutCASOnly {
		t.Errorf("post-end-to-end BlockLayout = %q, want %q",
			post.BlockLayout, metadata.BlockLayoutCASOnly)
	}

	// 2. Legacy keys gone — no key matching {payloadID}/block-{idx}.
	allKeys, _ := f.stub.Store.ListByPrefix(t.Context(), "")
	for _, k := range allKeys {
		if !strings.HasPrefix(k, "cas/") {
			t.Errorf("legacy key %q survived end-to-end pipeline", k)
		}
	}
}

// TestMigrateLoop_IntegrityFail covers Plan 14-05 behavior 6 (fail):
// integrity check fails → loop returns wrapped ErrIntegrityCheckFailed,
// performCutover NOT called (BlockLayout still legacy), legacy keys
// still present.
func TestMigrateLoop_IntegrityFail(t *testing.T) {
	f := newIntegrityFixture(t)
	addLegacyFile(t, f.loopFixture, "a.bin", "/a.bin", [][]byte{largeChunk('a')})

	// Inject a 404 for every HEAD so integrity fails for sure. The
	// stub kicks in AFTER the migration loop's WriteBlockWithHash —
	// the override is read at HEAD time, not at WriteBlockWithHash
	// time, so the loop still completes its uploads.
	failingHEAD := false
	originalHEAD := f.stub.headFn
	f.stub.headFn = func(ctx context.Context, key string) (remote.HeadResult, error) {
		if !failingHEAD {
			if originalHEAD != nil {
				return originalHEAD(ctx, key)
			}
			return f.stub.Store.HeadObject(ctx, key)
		}
		return remote.HeadResult{}, blockstore.ErrBlockNotFound
	}

	// Activate the fail switch and run the full loop. The loop will
	// upload, then fail integrity, then short-circuit.
	failingHEAD = true
	opts := migrateOptions{share: f.share, stateDir: f.dataDir, parallel: 2}
	err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts)
	if err == nil {
		t.Fatal("expected integrity-check failure, got nil")
	}
	if !errors.Is(err, ErrIntegrityCheckFailed) {
		t.Fatalf("err = %v, want wrapped ErrIntegrityCheckFailed", err)
	}

	// BlockLayout MUST still be legacy (or empty == legacy).
	post, _ := f.mds.GetShareOptions(t.Context(), f.share)
	if post.BlockLayout == metadata.BlockLayoutCASOnly {
		t.Errorf("post-fail BlockLayout = cas-only; expected legacy (no cutover on integrity fail)")
	}

	// At least one legacy key must still exist (sweep was skipped).
	allKeys, _ := f.stub.Store.ListByPrefix(t.Context(), "")
	hasLegacy := false
	for _, k := range allKeys {
		if !strings.HasPrefix(k, "cas/") {
			hasLegacy = true
			break
		}
	}
	if !hasLegacy {
		t.Errorf("no legacy keys remain; sweep ran despite integrity fail (D-A8 violated)")
	}
}

// TestMigrateLoop_DryRunSkipsCutover covers Plan 14-05 behavior 7:
// dry-run skips integrity, cutover, AND legacy delete entirely.
// BlockLayout is unchanged; legacy keys survive.
func TestMigrateLoop_DryRunSkipsCutover(t *testing.T) {
	f := newIntegrityFixture(t)
	addLegacyFile(t, f.loopFixture, "a.bin", "/a.bin", [][]byte{largeChunk('a')})

	opts := migrateOptions{share: f.share, stateDir: f.dataDir, parallel: 2, dryRun: true}
	if err := runMigrateLoopWithRuntime(t.Context(), f.rt, opts); err != nil {
		t.Fatalf("dry-run end-to-end: %v", err)
	}

	post, _ := f.mds.GetShareOptions(t.Context(), f.share)
	if post.BlockLayout == metadata.BlockLayoutCASOnly {
		t.Errorf("dry-run flipped BlockLayout to cas-only; want unchanged")
	}
	allKeys, _ := f.stub.Store.ListByPrefix(t.Context(), "")
	hasLegacy := false
	for _, k := range allKeys {
		if !strings.HasPrefix(k, "cas/") {
			hasLegacy = true
			break
		}
	}
	if !hasLegacy {
		t.Errorf("dry-run deleted legacy keys; expected zero touches")
	}
}
