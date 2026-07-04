package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// seedAuditTestStore creates a memory metadata store with one share named
// "audit-test" and three regular files. The first file has 5 ChunkRefs,
// the second has 3, the third has 2 (10 manifest refs total). Each
// manifest ref is backed by a FileChunk row whose ID is
// "{payloadID}/{offset}" with a matching non-zero hash, so the
// manifest↔FileChunk-row invariant holds (DanglingRefs == 0) pre-mutation.
//
// Returns the store, the share name, and per-file (payloadID, []offset)
// so a test can delete a specific backing row to manufacture a dangling
// manifest reference.
func seedAuditTestStore(t *testing.T) (store *metadatamemory.MemoryMetadataStore, shareName string, payloadIDs []string, offsets [][]uint64) {
	t.Helper()
	store = metadatamemory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	shareName = "audit-test"

	// Create share + root.
	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	// Three files with 5, 3, 2 ChunkRefs respectively.
	perFile := []int{5, 3, 2}
	now := time.Now().UTC()
	payloadIDs = make([]string, len(perFile))
	offsets = make([][]uint64, len(perFile))

	for fi, n := range perFile {
		name := fileNameFor(fi)
		handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
		if err != nil {
			t.Fatalf("GenerateHandle: %v", err)
		}
		_, fileID, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			t.Fatalf("DecodeFileHandle: %v", err)
		}

		// payloadID is the FileChunk-ID prefix; the engine keys blocks as
		// "{payloadID}/{offset}". Use a stable per-file payload id.
		payloadID := "payload-" + jstr(fi)
		payloadIDs[fi] = payloadID
		fileOffsets := make([]uint64, n)
		refs := make([]block.ChunkRef, n)
		for bi := 0; bi < n; bi++ {
			h := hashForFileChunk(fi, bi)
			off := uint64(bi) * 4096
			blockID := payloadID + "/" + jstr(int(off))
			fb := &block.FileChunk{
				ID:            blockID,
				Hash:          h,
				State:         block.BlockStatePending,
				LocalPath:     "/cache/" + blockID,
				BlockStoreKey: "cas/" + h.String()[0:2] + "/" + h.String()[2:4] + "/" + h.String(),
				DataSize:      4096,
				LastAccess:    now,
				CreatedAt:     now,
			}
			if err := store.Put(ctx, fb); err != nil {
				t.Fatalf("Put block %s: %v", blockID, err)
			}
			fileOffsets[bi] = off
			refs[bi] = block.ChunkRef{Hash: h, Offset: off, Size: 4096}
		}

		file := &metadata.File{
			ID:        fileID,
			ShareName: shareName,
			FileAttr: metadata.FileAttr{
				Type:      metadata.FileTypeRegular,
				Mode:      0o644,
				UID:       1000,
				GID:       1000,
				Mtime:     now,
				Ctime:     now,
				Atime:     now,
				PayloadID: metadata.PayloadID(payloadID),
				Blocks:    refs,
			},
		}
		if err := store.PutFile(ctx, file); err != nil {
			t.Fatalf("PutFile: %v", err)
		}
		if err := store.SetParent(ctx, handle, rootHandle); err != nil {
			t.Fatalf("SetParent: %v", err)
		}
		if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
			t.Fatalf("SetChild: %v", err)
		}
		if err := store.SetLinkCount(ctx, handle, 1); err != nil {
			t.Fatalf("SetLinkCount: %v", err)
		}
		offsets[fi] = fileOffsets
	}
	return store, shareName, payloadIDs, offsets
}

func fileNameFor(i int) string {
	return "file-" + jstr(i) + ".bin"
}

// jstr — int-to-string without importing strconv twice across the test
// fixture (keeps fixture isolated from the engine's strconv-heavy paths).
func jstr(i int) string {
	if i == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [20]byte
	pos := len(buf)
	for n := i; n > 0; n /= 10 {
		pos--
		buf[pos] = digits[n%10]
	}
	return string(buf[pos:])
}

// hashForFileChunk builds a deterministic non-zero ContentHash for a
// (fileIdx, blockIdx) pair so different files / blocks have distinct
// hashes. Mirrors storetest's hashOfSeed pattern.
func hashForFileChunk(fileIdx, blockIdx int) block.ContentHash {
	var h block.ContentHash
	seed := []byte("audit-fixture-f" + jstr(fileIdx) + "-b" + jstr(blockIdx))
	for i := 0; i < block.HashSize; i++ {
		h[i] = seed[i%len(seed)] ^ byte(i)
	}
	return h
}

// TestAuditRefcounts_Computes asserts AuditRefcounts returns the expected
// aggregate counts when the invariant holds. 3 files with 5+3+2=10
// manifest refs, each backed by a FileChunk row, yields DanglingRefs=0
// (Delta=0).
func TestAuditRefcounts_Computes(t *testing.T) {
	store, shareName, _, _ := seedAuditTestStore(t)
	tmpDir := t.TempDir()

	res, err := AuditRefcounts(context.Background(), shareName, store, tmpDir)
	if err != nil {
		t.Fatalf("AuditRefcounts: %v", err)
	}
	if res.Share != shareName {
		t.Errorf("Share = %q, want %q", res.Share, shareName)
	}
	if res.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want 3", res.TotalFiles)
	}
	if res.TotalRefs != 10 {
		t.Errorf("TotalRefs = %d, want 10", res.TotalRefs)
	}
	if res.BackedRefs != 10 {
		t.Errorf("BackedRefs = %d, want 10 (every manifest ref backed)", res.BackedRefs)
	}
	if res.DanglingRefs != 0 {
		t.Errorf("DanglingRefs = %d, want 0 (invariant must hold)", res.DanglingRefs)
	}
	if res.Delta != 0 {
		t.Errorf("Delta = %d, want 0 (invariant must hold)", res.Delta)
	}
	if res.StartedAt.IsZero() || res.CompletedAt.IsZero() {
		t.Errorf("timestamps not populated: %+v", res)
	}
	if res.DurationMS < 0 {
		t.Errorf("DurationMS = %d, want non-negative", res.DurationMS)
	}
}

// TestAuditRefcounts_DetectsDelta asserts that a genuine dangling
// reference surfaces in the audit's DanglingRefs/Delta fields. Deleting
// one FileChunk row (without touching FileAttr.Blocks) leaves the manifest
// entry pointing at a chunk the store no longer records — the
// silent-data-loss class. TotalRefs stays 10 (manifest unchanged), but
// BackedRefs drops to 9 and DanglingRefs == Delta == 1.
func TestAuditRefcounts_DetectsDelta(t *testing.T) {
	store, shareName, payloadIDs, offsets := seedAuditTestStore(t)
	tmpDir := t.TempDir()

	// Delete the FileChunk row backing file 0's first manifest ref, leaving
	// the manifest entry in place → that ref becomes dangling.
	danglingID := payloadIDs[0] + "/" + jstr(int(offsets[0][0]))
	if err := store.Delete(context.Background(), danglingID); err != nil {
		t.Fatalf("Delete backing row %q: %v", danglingID, err)
	}

	res, err := AuditRefcounts(context.Background(), shareName, store, tmpDir)
	if err != nil {
		t.Fatalf("AuditRefcounts: %v", err)
	}
	if res.TotalRefs != 10 {
		t.Errorf("TotalRefs = %d, want 10 (deleting a row does not change FileAttr.Blocks)", res.TotalRefs)
	}
	if res.BackedRefs != 9 {
		t.Errorf("BackedRefs = %d, want 9 (one backing row deleted)", res.BackedRefs)
	}
	if res.DanglingRefs != 1 {
		t.Errorf("DanglingRefs = %d, want 1 (the manifest ref with no backing row)", res.DanglingRefs)
	}
	if res.Delta != 1 {
		t.Errorf("Delta = %d, want 1 (Delta == DanglingRefs)", res.Delta)
	}
}

// TestAuditRefcounts_DetectsHashMismatch asserts that a backing row present
// at the right offset but carrying a DIFFERENT (non-zero) hash than the
// manifest ref is counted as DANGLING, not backed. This is genuine
// corruption: the store records a different chunk than the file claims at
// that offset, and crediting it as backed would silently hide the mismatch.
// The manifest is unchanged (TotalRefs stays 10), but the tampered ref drops
// BackedRefs to 9 and DanglingRefs == Delta == 1.
func TestAuditRefcounts_DetectsHashMismatch(t *testing.T) {
	store, shareName, payloadIDs, offsets := seedAuditTestStore(t)
	tmpDir := t.TempDir()
	ctx := context.Background()

	// Overwrite the FileChunk row backing file 0's first manifest ref with a
	// row at the SAME id/offset but a different non-zero hash, leaving the
	// manifest entry untouched → hash mismatch → that ref is dangling.
	mismatchID := payloadIDs[0] + "/" + jstr(int(offsets[0][0]))
	now := time.Now().UTC()
	wrongHash := hashForFileChunk(99, 99) // distinct from any seeded ref hash
	if wrongHash.IsZero() {
		t.Fatal("wrongHash must be non-zero to exercise the mismatch path")
	}
	wrongRow := &block.FileChunk{
		ID:            mismatchID,
		Hash:          wrongHash,
		State:         block.BlockStatePending,
		LocalPath:     "/cache/" + mismatchID,
		BlockStoreKey: "cas/" + wrongHash.String()[0:2] + "/" + wrongHash.String()[2:4] + "/" + wrongHash.String(),
		DataSize:      4096,
		LastAccess:    now,
		CreatedAt:     now,
	}
	if err := store.Put(ctx, wrongRow); err != nil {
		t.Fatalf("Put mismatched row %q: %v", mismatchID, err)
	}

	res, err := AuditRefcounts(ctx, shareName, store, tmpDir)
	if err != nil {
		t.Fatalf("AuditRefcounts: %v", err)
	}
	if res.TotalRefs != 10 {
		t.Errorf("TotalRefs = %d, want 10 (overwriting a row does not change FileAttr.Blocks)", res.TotalRefs)
	}
	if res.BackedRefs != 9 {
		t.Errorf("BackedRefs = %d, want 9 (the hash-mismatched ref is not backed)", res.BackedRefs)
	}
	if res.DanglingRefs != 1 {
		t.Errorf("DanglingRefs = %d, want 1 (hash mismatch must be detected as dangling)", res.DanglingRefs)
	}
	if res.Delta != 1 {
		t.Errorf("Delta = %d, want 1 (Delta == DanglingRefs)", res.Delta)
	}
}

// TestAuditRefcounts_PersistsLastRun asserts the audit writes
// <localStoreRoot>/audit-state/last-inv02.json containing the
// timestamps and counts. Subsequent runs OVERWRITE atomically.
func TestAuditRefcounts_PersistsLastRun(t *testing.T) {
	store, shareName, _, _ := seedAuditTestStore(t)
	tmpDir := t.TempDir()

	res, err := AuditRefcounts(context.Background(), shareName, store, tmpDir)
	if err != nil {
		t.Fatalf("AuditRefcounts: %v", err)
	}

	want := filepath.Join(tmpDir, "audit-state", "last-inv02.json")
	if AuditLastRunPath(tmpDir) != want {
		t.Errorf("AuditLastRunPath = %q, want %q", AuditLastRunPath(tmpDir), want)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read last-inv02.json: %v", err)
	}
	var got AuditRefcountsResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal last-inv02.json: %v", err)
	}
	if got.Share != res.Share {
		t.Errorf("persisted Share = %q, want %q", got.Share, res.Share)
	}
	if got.TotalFiles != res.TotalFiles || got.TotalRefs != res.TotalRefs || got.BackedRefs != res.BackedRefs || got.DanglingRefs != res.DanglingRefs || got.Delta != res.Delta {
		t.Errorf("persisted counts mismatch: got %+v, want %+v", got, res)
	}
	if got.StartedAt.IsZero() || got.CompletedAt.IsZero() {
		t.Errorf("persisted timestamps missing: %+v", got)
	}

	// Atomic-rename check: no .tmp file lingers after a successful run.
	if _, err := os.Stat(want + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("stray .tmp file: stat err = %v", err)
	}

	// Second run overwrites cleanly (empty store would still persist a
	// fresh summary; here we just verify a re-run does not error and
	// the file remains parsable).
	if _, err := AuditRefcounts(context.Background(), shareName, store, tmpDir); err != nil {
		t.Fatalf("AuditRefcounts (second run): %v", err)
	}
	data2, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}
	var got2 AuditRefcountsResult
	if err := json.Unmarshal(data2, &got2); err != nil {
		t.Fatalf("unmarshal after second run: %v", err)
	}
	if !got2.CompletedAt.After(got.CompletedAt) && !got2.CompletedAt.Equal(got.CompletedAt) {
		t.Errorf("second-run CompletedAt = %v, expected ≥ first-run %v", got2.CompletedAt, got.CompletedAt)
	}
}

// TestAuditRefcounts_EmptyLocalRoot asserts an empty localStoreRoot
// (in-memory backend) skips persistence cleanly without error.
func TestAuditRefcounts_EmptyLocalRoot(t *testing.T) {
	store, shareName, _, _ := seedAuditTestStore(t)

	res, err := AuditRefcounts(context.Background(), shareName, store, "")
	if err != nil {
		t.Fatalf("AuditRefcounts (empty root): %v", err)
	}
	if res.TotalRefs != 10 || res.BackedRefs != 10 || res.DanglingRefs != 0 || res.Delta != 0 {
		t.Errorf("counts mismatch on empty root: %+v", res)
	}
	if AuditLastRunPath("") != "" {
		t.Errorf("AuditLastRunPath(\"\") = %q, want empty", AuditLastRunPath(""))
	}
}

// TestAuditRefcounts_NilStore returns an error rather than panicking.
func TestAuditRefcounts_NilStore(t *testing.T) {
	_, err := AuditRefcounts(context.Background(), "any", nil, "")
	if err == nil {
		t.Fatal("expected error on nil store, got nil")
	}
}

// TestAuditRefcounts_EmptyShare returns an error rather than walking
// every share. Defends against handler bugs that pass through "".
func TestAuditRefcounts_EmptyShare(t *testing.T) {
	store := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	_, err := AuditRefcounts(context.Background(), "", store, "")
	if err == nil {
		t.Fatal("expected error on empty share, got nil")
	}
}
