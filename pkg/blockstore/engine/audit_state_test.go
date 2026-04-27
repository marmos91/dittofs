package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// seedAuditTestStore creates a memory metadata store with one share named
// "audit-test" and three regular files. The first file has 5 BlockRefs,
// the second has 3, the third has 2. Each FileBlock is seeded with a
// matching RefCount=1 so the pre-leak invariant ∑RefCount == ∑len(Blocks)
// holds (3+5+2 = 10 refs, 10 blocks × RefCount=1). Used by all three
// audit tests below.
func seedAuditTestStore(t *testing.T) (*metadatamemory.MemoryMetadataStore, string, [][]string) {
	t.Helper()
	store := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	const shareName = "audit-test"

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

	// Three files with 5, 3, 2 BlockRefs respectively.
	perFile := []int{5, 3, 2}
	now := time.Now().UTC()
	allBlockIDs := make([][]string, len(perFile))

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

		blockIDs := make([]string, n)
		refs := make([]blockstore.BlockRef, n)
		for bi := 0; bi < n; bi++ {
			h := hashForFileBlock(fi, bi)
			blockID := fileNameFor(fi) + "/" + jstr(bi)
			fb := &blockstore.FileBlock{
				ID:            blockID,
				Hash:          h,
				State:         blockstore.BlockStateRemote,
				LocalPath:     "/cache/" + blockID,
				BlockStoreKey: "cas/" + h.String()[0:2] + "/" + h.String()[2:4] + "/" + h.String(),
				DataSize:      4096,
				RefCount:      1,
				LastAccess:    now,
				CreatedAt:     now,
			}
			if err := store.Put(ctx, fb); err != nil {
				t.Fatalf("Put block %s: %v", blockID, err)
			}
			blockIDs[bi] = blockID
			refs[bi] = blockstore.BlockRef{Hash: h, Offset: uint64(bi) * 4096, Size: 4096}
		}

		file := &metadata.File{
			ID:        fileID,
			ShareName: shareName,
			FileAttr: metadata.FileAttr{
				Type:   metadata.FileTypeRegular,
				Mode:   0o644,
				UID:    1000,
				GID:    1000,
				Mtime:  now,
				Ctime:  now,
				Atime:  now,
				Blocks: refs,
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
		allBlockIDs[fi] = blockIDs
	}
	return store, shareName, allBlockIDs
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

// hashForFileBlock builds a deterministic non-zero ContentHash for a
// (fileIdx, blockIdx) pair so different files / blocks have distinct
// hashes. Mirrors storetest's hashOfSeed pattern.
func hashForFileBlock(fileIdx, blockIdx int) blockstore.ContentHash {
	var h blockstore.ContentHash
	seed := []byte("audit-fixture-f" + jstr(fileIdx) + "-b" + jstr(blockIdx))
	for i := 0; i < blockstore.HashSize; i++ {
		h[i] = seed[i%len(seed)] ^ byte(i)
	}
	return h
}

// TestAuditRefcounts_Computes asserts AuditRefcounts returns the expected
// aggregate counts when the invariant holds. 3 files with 5+3+2=10
// BlockRefs each carrying RefCount=1 yields delta=0.
func TestAuditRefcounts_Computes(t *testing.T) {
	store, shareName, _ := seedAuditTestStore(t)
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
	if res.TotalRefCount != 10 {
		t.Errorf("TotalRefCount = %d, want 10", res.TotalRefCount)
	}
	if res.Delta != 0 {
		t.Errorf("Delta = %d, want 0 (invariant must hold pre-leak)", res.Delta)
	}
	if res.StartedAt.IsZero() || res.CompletedAt.IsZero() {
		t.Errorf("timestamps not populated: %+v", res)
	}
	if res.DurationMS < 0 {
		t.Errorf("DurationMS = %d, want non-negative", res.DurationMS)
	}
}

// TestAuditRefcounts_DetectsDelta asserts that a known refcount leak
// surfaces in the audit's Delta field. Bumping one block's RefCount by
// 5 (without touching FileAttr.Blocks anywhere) yields TotalRefCount=15
// vs TotalRefs=10 → Delta = -5.
func TestAuditRefcounts_DetectsDelta(t *testing.T) {
	store, shareName, allBlockIDs := seedAuditTestStore(t)
	tmpDir := t.TempDir()

	// Bump RefCount by 5 on the first block of file 0.
	const leak uint32 = 5
	if err := store.InjectRefCountLeak(context.Background(), allBlockIDs[0][0], leak); err != nil {
		t.Fatalf("InjectRefCountLeak: %v", err)
	}

	res, err := AuditRefcounts(context.Background(), shareName, store, tmpDir)
	if err != nil {
		t.Fatalf("AuditRefcounts: %v", err)
	}
	if res.TotalRefs != 10 {
		t.Errorf("TotalRefs = %d, want 10 (leak does not change FileAttr.Blocks)", res.TotalRefs)
	}
	if res.TotalRefCount != 10+uint64(leak) {
		t.Errorf("TotalRefCount = %d, want %d (leak adds to RefCount)", res.TotalRefCount, 10+leak)
	}
	wantDelta := -int64(leak)
	if res.Delta != wantDelta {
		t.Errorf("Delta = %d, want %d (refs - refcount)", res.Delta, wantDelta)
	}
}

// TestAuditRefcounts_PersistsLastRun asserts the audit writes
// <localStoreRoot>/audit-state/last-inv02.json containing the
// timestamps and counts. Subsequent runs OVERWRITE atomically.
func TestAuditRefcounts_PersistsLastRun(t *testing.T) {
	store, shareName, _ := seedAuditTestStore(t)
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
	if got.TotalFiles != res.TotalFiles || got.TotalRefs != res.TotalRefs || got.TotalRefCount != res.TotalRefCount || got.Delta != res.Delta {
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
	store, shareName, _ := seedAuditTestStore(t)

	res, err := AuditRefcounts(context.Background(), shareName, store, "")
	if err != nil {
		t.Fatalf("AuditRefcounts (empty root): %v", err)
	}
	if res.TotalRefs != 10 || res.TotalRefCount != 10 || res.Delta != 0 {
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
