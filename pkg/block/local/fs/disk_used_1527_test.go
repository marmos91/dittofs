package fs

// Regression tests for #1527: dittofs_localstore_disk_used_bytes underflowed
// to negative and the local tier grew unbounded past --local-store-size.
//
// Three asymmetries conspired:
//
//  1. Recover seeded diskUsed from .blk files only — legacy CAS chunk files
//     (blocks/<hh>/<hh>/<hex>) were registered as LRU eviction candidates by
//     seedLRUFromDisk but their bytes were never counted, so a post-restart
//     GC delete / eviction subtracted bytes that were never added.
//  2. Log-blob bytes (the post blocks-flip live write path) were accounted in
//     logBlobDiskUsed, which nothing read: not Stats, not ensureSpace. The
//     bulk of the tier was invisible to the disk limit.
//  3. Nothing could reclaim log-blob bytes, so even with correct accounting
//     the limit was unenforceable for blob-resident data.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newBlobStoreWithLimit builds an FSStore with the log-blob substrate wired
// (LocalChunkIndex) plus a real SyncedHashStore, mimicking the production
// post-flip configuration.
func newBlobStoreWithLimit(t *testing.T, dir string, maxDisk int64, mds *memory.MemoryMetadataStore) *FSStore {
	t.Helper()
	bc, err := NewWithOptions(dir, maxDisk, mds, FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(block.RetentionLRU, 0)
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// sumBlobFiles returns the total size of *.blob files under <dir>/blobs.
func sumBlobFiles(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read blobs dir: %v", err)
	}
	var total int64
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".blob" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			t.Fatalf("blob info: %v", err)
		}
		total += fi.Size()
	}
	return total
}

// TestLogBlobBytes_CountedAndSeededAcrossReopen asserts that log-blob bytes
// (the post-flip live write path) are (a) counted into DiskUsed as they are
// appended, (b) equal to the physical bytes on disk, and (c) re-seeded from
// the physical blob files when the store is reopened — before the fix they
// were invisible to Stats and reset to zero on every restart.
func TestLogBlobBytes_CountedAndSeededAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds)
	ctx := context.Background()

	payloads := [][]byte{
		bytes.Repeat([]byte{0x01}, 100),
		bytes.Repeat([]byte{0x02}, 200),
		bytes.Repeat([]byte{0x03}, 300),
	}
	var hashes []block.ContentHash
	for _, p := range payloads {
		h := blake3ContentHash(p)
		if err := bc.StoreChunk(ctx, h, p); err != nil {
			t.Fatalf("StoreChunk: %v", err)
		}
		hashes = append(hashes, h)
	}

	if got := bc.Stats().DiskUsed; got != 600 {
		t.Fatalf("DiskUsed = %d, want 600 (log-blob bytes must be counted)", got)
	}

	if err := bc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Physical check after Close: with the write handle closed the directory
	// metadata is reliable on every OS (Windows reports stale sizes for files
	// with open handles).
	if phys := sumBlobFiles(t, dir); phys != 600 {
		t.Fatalf("physical blob bytes = %d, want 600 (counter must match disk)", phys)
	}

	// Reopen over the same dir + index: the counter must be re-seeded from
	// the physical blob files, not restart at zero.
	bc2 := newBlobStoreWithLimit(t, dir, 0, mds)
	if got := bc2.Stats().DiskUsed; got != 600 {
		t.Fatalf("post-reopen DiskUsed = %d, want 600 (seed from physical blobs)", got)
	}
	got, err := bc2.ReadChunk(ctx, hashes[2])
	if err != nil {
		t.Fatalf("ReadChunk after reopen: %v", err)
	}
	if !bytes.Equal(got, payloads[2]) {
		t.Fatalf("ReadChunk after reopen returned wrong bytes")
	}
}

// TestEnsureSpace_EvictsSealedSyncedBlob asserts that when the store exceeds
// maxDisk and the CAS LRU has nothing to give, ensureSpace evicts the oldest
// sealed, fully-synced log blob — enforcing --local-store-size for
// blob-resident data (#1527: eviction never fired).
func TestEnsureSpace_EvictsSealedSyncedBlob(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 600, mds)
	ctx := context.Background()

	// 400 bytes into the active blob, mirrored, then sealed.
	oldData := bytes.Repeat([]byte{0x10}, 400)
	oldHash := blake3ContentHash(oldData)
	if err := bc.StoreChunk(ctx, oldHash, oldData); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	if err := mds.MarkSynced(ctx, oldHash, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	oldLoc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, oldHash)
	if err != nil || !ok {
		t.Fatalf("GetLocalLocation(old): ok=%v err=%v", ok, err)
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Put (the read-through staging entry) reserves space via ensureSpace:
	// 400 used + 300 needed > 600 → must evict the sealed blob.
	newData := bytes.Repeat([]byte{0x20}, 300)
	newHash := blake3ContentHash(newData)
	if err := bc.Put(ctx, newHash, newData); err != nil {
		t.Fatalf("Put over limit: %v (blob eviction should have freed space)", err)
	}

	if got := bc.Stats().DiskUsed; got != 300 {
		t.Fatalf("DiskUsed after blob eviction = %d, want 300", got)
	}
	// The sealed blob's file must be unlinked. (Presence check, not a size
	// sum: the new active blob has an open write handle, and Windows reports
	// stale directory sizes for open files.)
	evictedPath := filepath.Join(dir, "blobs", oldLoc.LogBlobID+".blob")
	if _, statErr := os.Stat(evictedPath); !os.IsNotExist(statErr) {
		t.Fatalf("sealed blob %s still on disk (stat err=%v), want unlinked", evictedPath, statErr)
	}

	// The evicted chunk's index entry was pruned: clean local miss, so the
	// engine refetches from the remote (where it is synced).
	if _, err := bc.ReadChunk(ctx, oldHash); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadChunk(evicted) = %v, want ErrChunkNotFound", err)
	}
	got, err := bc.ReadChunk(ctx, newHash)
	if err != nil {
		t.Fatalf("ReadChunk(new): %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Fatalf("ReadChunk(new) returned wrong bytes")
	}
}

// TestEnsureSpace_RefusesUnsyncedBlob asserts the data-safety guard: a sealed
// blob whose chunks were never mirrored must NOT be evicted — evicting it
// would destroy the only copy. ensureSpace back-pressures and fails with
// ErrDiskFull instead.
func TestEnsureSpace_RefusesUnsyncedBlob(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 600, mds)
	bc.evictMaxWait = 50 * time.Millisecond // avoid the 30s back-pressure wait
	ctx := context.Background()

	unsynced := bytes.Repeat([]byte{0x30}, 400)
	h := blake3ContentHash(unsynced)
	if err := bc.StoreChunk(ctx, h, unsynced); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	newData := bytes.Repeat([]byte{0x40}, 300)
	if err := bc.Put(ctx, blake3ContentHash(newData), newData); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Put over limit with unsynced blob: got %v, want ErrDiskFull", err)
	}

	// The unsynced blob must remain readable and the counter untouched.
	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk(unsynced) after refused eviction: %v", err)
	}
	if !bytes.Equal(got, unsynced) {
		t.Fatalf("ReadChunk(unsynced) returned wrong bytes")
	}
	if used := bc.Stats().DiskUsed; used != 400 {
		t.Fatalf("DiskUsed = %d, want 400 (nothing evicted, nothing negative)", used)
	}
}

// TestReclaimSpace_EvictsBlobsWhenOverLimit asserts the write-path reclaim
// (invoked after each rollup pass): with the store over maxDisk, sealed
// synced blobs are evicted until back under the limit, while the active blob
// is never touched.
func TestReclaimSpace_EvictsBlobsWhenOverLimit(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 600, mds)
	ctx := context.Background()

	oldData := bytes.Repeat([]byte{0x50}, 400)
	oldHash := blake3ContentHash(oldData)
	if err := bc.StoreChunk(ctx, oldHash, oldData); err != nil {
		t.Fatalf("StoreChunk(old): %v", err)
	}
	if err := mds.MarkSynced(ctx, oldHash, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// 300 more into the (new) active blob: used=700 > 600.
	liveData := bytes.Repeat([]byte{0x60}, 300)
	liveHash := blake3ContentHash(liveData)
	if err := bc.StoreChunk(ctx, liveHash, liveData); err != nil {
		t.Fatalf("StoreChunk(live): %v", err)
	}
	if got := bc.Stats().DiskUsed; got != 700 {
		t.Fatalf("DiskUsed = %d, want 700 before reclaim", got)
	}

	bc.reclaimSpace(ctx)

	if got := bc.Stats().DiskUsed; got != 300 {
		t.Fatalf("DiskUsed after reclaim = %d, want 300 (sealed blob evicted)", got)
	}
	// The active blob (holding the unsynced live chunk) must survive.
	got, err := bc.ReadChunk(ctx, liveHash)
	if err != nil {
		t.Fatalf("ReadChunk(live) after reclaim: %v", err)
	}
	if !bytes.Equal(got, liveData) {
		t.Fatalf("ReadChunk(live) returned wrong bytes")
	}
	if _, err := bc.ReadChunk(ctx, oldHash); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadChunk(old) = %v, want ErrChunkNotFound", err)
	}
}

// TestBlobEvictOne_UnlinkFailureOrphanStaysCounted pins the accounting
// around EvictBlob's idempotency edge: after a failed unlink the blob is
// evicted in-memory but its file is still physically on disk, and later
// EvictBlob calls answer nil without retrying the remove. blobEvictOne must
// therefore (a) never subtract bytes that are still on disk (undercounting
// would re-break maxDisk enforcement), (b) never subtract the same blob
// twice, and (c) restore truthful accounting after a restart, when the seed
// walks the physical files and a fresh manager retries the unlink.
func TestBlobEvictOne_UnlinkFailureOrphanStaysCounted(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 600, mds)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x80}, 400)
	h := blake3ContentHash(data)
	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	if err := mds.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Inject an unlink failure: EvictBlob marks the blob evicted in-memory
	// and closes the fd, but os.Remove fails (write-protected directory), so
	// the orphan file stays on disk and listed by ListBlobs.
	if runtime.GOOS == "windows" {
		t.Skip("directory write-protection cannot block deletes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("directory write-protection does not apply to root")
	}
	blobsDir := filepath.Join(dir, "blobs")
	if err := os.Chmod(blobsDir, 0o555); err != nil {
		t.Fatalf("chmod blobs dir read-only: %v", err)
	}
	restore := func() { _ = os.Chmod(blobsDir, 0o755) }
	t.Cleanup(restore)

	// First attempt: unlink fails → error, and the counter must NOT move
	// (the bytes were not reclaimed).
	if freed, err := bc.blobEvictOne(ctx); err == nil || freed != 0 {
		t.Fatalf("blobEvictOne with unlink blocked = (%d, %v), want (0, error)", freed, err)
	}
	if got := bc.Stats().DiskUsed; got != 400 {
		t.Fatalf("DiskUsed after failed unlink = %d, want 400 (unchanged)", got)
	}

	// Retry: EvictBlob answers nil for the already-evicted blob WITHOUT
	// retrying the unlink — the file is still physically on disk, so the
	// bytes must STAY counted (no decrement) and the blob must be marked
	// processed rather than reported as freed space.
	if freed, err := bc.blobEvictOne(ctx); !errors.Is(err, errLRUEmpty) || freed != 0 {
		t.Fatalf("retry blobEvictOne = (%d, %v), want (0, errLRUEmpty)", freed, err)
	}
	if got := bc.Stats().DiskUsed; got != 400 {
		t.Fatalf("DiskUsed after orphan retry = %d, want 400 (file still on disk)", got)
	}

	// Every further call must skip the orphan via blobEvictedIDs: no second
	// subtraction, no clamp churn, counter still matches the physical file.
	if freed, err := bc.blobEvictOne(ctx); !errors.Is(err, errLRUEmpty) || freed != 0 {
		t.Fatalf("post-orphan blobEvictOne = (%d, %v), want (0, errLRUEmpty)", freed, err)
	}
	if got := bc.Stats().DiskUsed; got != 400 {
		t.Fatalf("DiskUsed after skip = %d, want 400", got)
	}

	// Restart self-heal: reopen over the same dir — the seed walks the
	// physical blob files (orphan included), so accounting stays truthful.
	restore()
	if err := bc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	bc2 := newBlobStoreWithLimit(t, dir, 600, mds)
	if got := bc2.Stats().DiskUsed; got != 400 {
		t.Fatalf("post-reopen DiskUsed = %d, want 400 (orphan re-seeded)", got)
	}
}

// TestReadChunk_DanglingEntryAfterBlobEviction_SelfHeals covers the lazy
// cleanup path for blobs evicted without eager index pruning (e.g. blobs
// written by a previous process): a read hitting an evicted blob must drop
// the dangling index entry and report a clean miss, routing the engine to
// refetch from the remote.
func TestReadChunk_DanglingEntryAfterBlobEviction_SelfHeals(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x70}, 128)
	h := blake3ContentHash(data)
	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Evict the sealed blob directly through the manager, bypassing
	// blobEvictOne's eager index cleanup — the index entry now dangles.
	loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
	if err != nil || !ok {
		t.Fatalf("GetLocalLocation: ok=%v err=%v", ok, err)
	}
	if err := bc.logBlob.EvictBlob(ctx, loc.LogBlobID, func(string) bool { return true }); err != nil {
		t.Fatalf("EvictBlob: %v", err)
	}

	// First read: dangling entry → dropped → clean miss.
	if _, err := bc.ReadChunk(ctx, h); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadChunk(dangling) = %v, want ErrChunkNotFound", err)
	}
	// The entry is gone, so existence checks now agree (a re-stage would
	// re-append instead of dedup-skipping against a dead entry).
	exists, err := bc.HasChunk(ctx, h)
	if err != nil {
		t.Fatalf("HasChunk: %v", err)
	}
	if exists {
		t.Fatalf("HasChunk = true after self-heal, want false")
	}
}
