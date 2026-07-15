package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/compression"
	"github.com/marmos91/dittofs/pkg/block/encryption"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// carveFixture wires an FSStore backed by a log-blob substrate, a memory
// metadata store (the blockCommitter: Transactor + SyncedHashStore +
// LocalChunkIndex), and the provided block-keyed remote into a Syncer with the
// carve substrate fully active. carveBytes sizes the block target.
type carveFixture struct {
	local  *fs.FSStore
	ms     *metadatamemory.MemoryMetadataStore
	remote remote.RemoteBlockStore
	syncer *Syncer
}

func newCarveFixture(t *testing.T, rbs remote.RemoteStore, carveBytes int64) *carveFixture {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cfg := DefaultConfig()
	cfg.BlockCarveBytes = carveBytes
	cfg.ManualSync = true // explicit carveFlush only; no background goroutine racing assertions

	syncer := NewSyncer(local, rbs, ms, cfg)
	syncer.SetSyncedHashStore(ms)
	rblock, ok := rbs.(remote.RemoteBlockStore)
	if !ok {
		t.Fatalf("remote does not implement RemoteBlockStore")
	}
	syncer.SetRemoteBlockStore(rblock)

	if !syncer.carveActive.Load() {
		t.Fatal("carve substrate should be active after wiring")
	}
	return &carveFixture{local: local, ms: ms, remote: rblock, syncer: syncer}
}

// storeChunk writes a chunk to the local log-blob tier and registers it for
// carve exactly as the onChunkComplete hook would.
func (f *carveFixture) storeChunk(t *testing.T, ctx context.Context, data []byte) block.ContentHash {
	t.Helper()
	h := block.ContentHash(blake3.Sum256(data))
	if err := f.local.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	f.syncer.addPendingHash(h, int64(len(data)))
	return h
}

// countRemoteBlocks returns the number of blocks/ objects on the memory remote.
func countRemoteBlocks(t *testing.T, ctx context.Context, s *remotememory.Store) int {
	t.Helper()
	n := 0
	if err := s.WalkBlocks(ctx, func(string, block.Meta) error { n++; return nil }); err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}
	return n
}

// countRemoteCAS returns the number of cas/ objects on the memory remote.
func countRemoteCAS(t *testing.T, ctx context.Context, s *remotememory.Store) int {
	t.Helper()
	n := 0
	if err := s.Walk(ctx, func(block.ContentHash, block.Meta) error { n++; return nil }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return n
}

// TestCarve_NewWriteProducesBlockNotCAS is DoD test (1): a new write through the
// carve path produces a blocks/ object on the remote and NO cas/ object, the
// chunk is marked synced with a block locator, and the chunk reads back
// byte-identical (from the local log-blob tier and from the remote block).
func TestCarve_NewWriteProducesBlockNotCAS(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("carve-me-"), 512)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	if got := countRemoteBlocks(t, ctx, mem); got != 1 {
		t.Fatalf("remote blocks = %d, want 1", got)
	}
	if got := countRemoteCAS(t, ctx, mem); got != 0 {
		t.Fatalf("remote cas objects = %d, want 0 (carve must not write cas/)", got)
	}

	// The chunk is marked synced with a BLOCK locator (BlockID set).
	loc, synced, err := f.ms.GetLocator(ctx, h)
	if err != nil {
		t.Fatalf("GetLocator: %v", err)
	}
	if !synced {
		t.Fatal("chunk should be marked synced after carve")
	}
	if loc.IsStandalone() {
		t.Fatalf("carve must record a block locator, got standalone: %+v", loc)
	}

	// Read back via the remote block using the persisted locator.
	cr, ok := f.remote.(remote.ChunkReader)
	if !ok {
		t.Fatal("memory remote should implement ChunkReader")
	}
	got, err := cr.ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("remote block round-trip mismatch")
	}

	// Local read still returns the bytes (log-blob tier).
	localGot, err := f.local.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("local ReadChunk: %v", err)
	}
	if !bytes.Equal(localGot, data) {
		t.Fatal("local log-blob read mismatch")
	}

	// The carve set is drained.
	if n := f.syncer.CarvePendingCount(); n != 0 {
		t.Fatalf("carve pending = %d, want 0 after flush", n)
	}
}

// TestCarve_ThroughCompressedRemote is DoD test (2) (compression layer): the
// block's per-chunk wire bytes are sealed (compressed, != plaintext) and decode
// back byte-identical through the decorated remote's ReadChunk.
func TestCarve_ThroughCompressedRemote(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	dec, err := compression.NewRemote(base, compression.CompressionPolicy{Algo: compression.AlgoZstd})
	if err != nil {
		t.Fatalf("compression.NewRemote: %v", err)
	}
	f := newCarveFixture(t, dec, DefaultBlockCarveBytes)

	// Highly compressible payload so the sealed wire is strictly smaller.
	data := bytes.Repeat([]byte("AAAA-BBBB-CCCC-DDDD-"), 4096)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	loc, synced, err := f.ms.GetLocator(ctx, h)
	if err != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, err)
	}

	// The wire body inside the block must be smaller than the plaintext —
	// proof the compression seal ran during carve (not stored plaintext).
	if loc.WireLength >= int64(len(data)) {
		t.Fatalf("carved wire body not compressed: wireLen=%d plaintext=%d", loc.WireLength, len(data))
	}

	cr := f.remote.(remote.ChunkReader)
	got, err := cr.ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("decoded carve chunk mismatch through compressed remote")
	}
}

// newEncryptionProvider builds a local key-file provider for carve tests.
func newEncryptionProvider(t *testing.T) keyprovider.KeyProvider {
	t.Helper()
	raw, err := keyprovider.GenerateKeyFile("carve-test-passphrase")
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	path := filepath.Join(t.TempDir(), "share.key")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", "carve-test-passphrase")
	p, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestCarve_ThroughCompressEncryptRemote is DoD test (2) (full stack): carving
// through a compression(encryption(base)) remote seals each chunk (the raw
// block object does not contain the plaintext) and decodes back byte-identical
// through the decorated remote's ReadChunk — proving no plaintext at rest on
// an encrypted share.
func TestCarve_ThroughCompressEncryptRemote(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	enc, err := encryption.NewRemote(base, encryption.EncryptionPolicy{AEAD: encryption.AEADAES256GCM}, newEncryptionProvider(t))
	if err != nil {
		t.Fatalf("encryption.NewRemote: %v", err)
	}
	dec, err := compression.NewRemote(enc, compression.CompressionPolicy{Algo: compression.AlgoZstd})
	if err != nil {
		t.Fatalf("compression.NewRemote: %v", err)
	}
	f := newCarveFixture(t, dec, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("secret-payload-block-"), 1024)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}
	if got := countRemoteBlocks(t, ctx, base); got != 1 {
		t.Fatalf("remote blocks = %d, want 1", got)
	}

	loc, synced, err := f.ms.GetLocator(ctx, h)
	if err != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, err)
	}

	// Inspect the raw block bytes on the BASE store: the sealed wire body must
	// not contain the plaintext, confirming the encryption seal ran during carve.
	rawBlock, err := base.GetBlock(ctx, loc.BlockID)
	if err != nil {
		t.Fatalf("base GetBlock: %v", err)
	}
	if bytes.Contains(rawBlock, data) {
		t.Fatal("encrypted carve must not store plaintext in the block object")
	}

	// Round-trip decode through the full decorated stack.
	cr := f.remote.(remote.ChunkReader)
	got, err := cr.ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("decoded carve chunk mismatch through compress+encrypt remote")
	}
}

// TestCarve_UnwiredSubstrateKeepsChunksPending pins the post-#1493 contract
// that replaced the deleted legacy mirror fallback: when the carve substrate is
// not wired (no RemoteBlockStore here; a missing blockCommitter behaves the
// same), carve is inactive, stored chunks stay in the single pending-carve set
// (unsyncedBytes charged), nothing is uploaded, and Flush honestly reports
// Finalized=false instead of claiming durability.
func TestCarve_UnwiredSubstrateKeepsChunksPending(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	mem := remotememory.New()
	syncer := NewSyncer(local, mem, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)
	// Deliberately NO SetRemoteBlockStore: the carve substrate stays unwired.

	if syncer.carveActive.Load() {
		t.Fatal("carve must be inactive without a RemoteBlockStore")
	}

	data := bytes.Repeat([]byte("stranded-"), 128)
	h := block.ContentHash(blake3.Sum256(data))
	if err := local.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	syncer.addPendingHash(h, int64(len(data)))

	// Post-#1493 there is only ONE pending set: the chunk lands in it even
	// with carve inactive (the legacy mirror set is gone).
	if n := syncer.CarvePendingCount(); n != 1 {
		t.Fatalf("carve pending = %d, want 1 (single pending set)", n)
	}
	if got := syncer.UnsyncedBytes(); got != int64(len(data)) {
		t.Fatalf("UnsyncedBytes = %d, want %d", got, len(data))
	}

	// carveFlush is a no-op while inactive: no error, nothing claimed.
	if err := syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush (inactive): %v", err)
	}
	if n := syncer.CarvePendingCount(); n != 1 {
		t.Fatalf("carve pending after inactive flush = %d, want 1 (chunk must stay pending)", n)
	}
	if got := syncer.UnsyncedBytes(); got != int64(len(data)) {
		t.Fatalf("UnsyncedBytes after inactive flush = %d, want %d", got, len(data))
	}
	if got := countRemoteBlocks(t, ctx, mem); got != 0 {
		t.Fatalf("remote blocks = %d, want 0 (nothing may upload while unwired)", got)
	}
	if _, synced, err := ms.GetLocator(ctx, h); err != nil || synced {
		t.Fatalf("GetLocator: synced=%v err=%v, want unsynced with no error", synced, err)
	}

	// Flush must report the soft condition instead of claiming durability.
	res, err := syncer.Flush(ctx, "share/stranded")
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if res.Finalized {
		t.Fatal("Flush.Finalized = true, want false while the carve substrate is unwired")
	}
}

// failOnNthMarkSynced is a blockCommitter wrapper that injects a MarkSynced
// failure on a specific call (1-indexed) INSIDE the commit transaction —
// DefaultCommitBlock marks chunks synced via tx.MarkSynced, so the wrapper
// intercepts WithTransaction and hands the closure a fault-injecting
// Transaction. Used by TestCarve_CommitFailureRollsBackAtomically to simulate
// a transient metadata-store blip mid-commit.
type failOnNthMarkSynced struct {
	*metadatamemory.MemoryMetadataStore
	mu         sync.Mutex
	count      int
	failOnCall int // which call (1-indexed) to fail
}

func (f *failOnNthMarkSynced) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	return f.MemoryMetadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return fn(&failNthMarkSyncedTx{Transaction: tx, parent: f})
	})
}

type failNthMarkSyncedTx struct {
	metadata.Transaction
	parent *failOnNthMarkSynced
}

func (t *failNthMarkSyncedTx) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	t.parent.mu.Lock()
	t.parent.count++
	n := t.parent.count
	t.parent.mu.Unlock()
	if n == t.parent.failOnCall {
		return errors.New("injected MarkSynced failure")
	}
	return t.Transaction.MarkSynced(ctx, hash, loc)
}

// TestCarve_BoundaryAtBlockCarveBytes is DoD test (3): with a small carve
// target, enough chunks to exceed several blocks produce multiple blocks via the
// size-triggered (drainAll=false) path, and a trailing partial flushes only on
// drainAll=true (idle/Flush).
func TestCarve_BoundaryAtBlockCarveBytes(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	const chunkSize = 4096
	const carveBytes = 3 * chunkSize // a full block holds 3 chunks
	f := newCarveFixture(t, mem, carveBytes)

	// 7 chunks => 2 full blocks (6 chunks) + 1 partial chunk left over.
	hashes := make([]block.ContentHash, 0, 7)
	for i := 0; i < 7; i++ {
		data := bytes.Repeat([]byte{byte('a' + i)}, chunkSize)
		// make each distinct
		data = append(data, []byte(fmt.Sprintf("-%d", i))...)
		hashes = append(hashes, f.storeChunk(t, ctx, data))
	}

	// Size-triggered: only full blocks emit; the sub-target remainder stays.
	if err := f.syncer.carveFlush(ctx, false); err != nil {
		t.Fatalf("carveFlush(size): %v", err)
	}
	if got := countRemoteBlocks(t, ctx, mem); got != 2 {
		t.Fatalf("after size flush: blocks = %d, want 2 full blocks", got)
	}
	if n := f.syncer.CarvePendingCount(); n != 1 {
		t.Fatalf("after size flush: carve pending = %d, want 1 (partial remainder)", n)
	}

	// Idle/explicit drain: the partial block flushes.
	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush(drainAll): %v", err)
	}
	if got := countRemoteBlocks(t, ctx, mem); got != 3 {
		t.Fatalf("after drain flush: blocks = %d, want 3", got)
	}
	if n := f.syncer.CarvePendingCount(); n != 0 {
		t.Fatalf("after drain flush: carve pending = %d, want 0", n)
	}

	// Every chunk is synced with a block locator.
	for _, h := range hashes {
		loc, synced, err := f.ms.GetLocator(ctx, h)
		if err != nil || !synced {
			t.Fatalf("hash %s: synced=%v err=%v", h, synced, err)
		}
		if loc.IsStandalone() {
			t.Fatalf("hash %s: want block locator, got standalone", h)
		}
	}
}

// TestCarve_CommitFailureRollsBackAtomically verifies DefaultCommitBlock's
// atomic semantics from the carver's side: a MarkSynced failure INSIDE the
// commit transaction rolls back the whole commit (no block record, no synced
// markers), carveFlush surfaces the error and requeues the batch, and the
// next flush re-carves the chunks into a fresh block that commits fully. The
// first upload becomes an orphan remote block object for GC to reclaim.
func TestCarve_CommitFailureRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()

	// Wire a real memory store whose transaction fails MarkSynced exactly once
	// (call #2 — mid-commit, after the first chunk already marked) to simulate
	// a transient metadata-store blip inside the commit transaction.
	realMS := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	wrapper := &failOnNthMarkSynced{
		MemoryMetadataStore: realMS,
		failOnCall:          2, // second chunk's tx.MarkSynced fails once
	}

	local, err := fs.NewWithOptions(t.TempDir(), 0, realMS, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cfg := DefaultConfig()
	cfg.BlockCarveBytes = DefaultBlockCarveBytes
	cfg.ManualSync = true

	syncer := NewSyncer(local, mem, realMS, cfg)
	syncer.SetSyncedHashStore(wrapper) // blockCommitter = wrapper (intercepts tx.MarkSynced)
	var memRBS remote.RemoteBlockStore = mem
	syncer.SetRemoteBlockStore(memRBS)

	if !syncer.carveActive.Load() {
		t.Fatal("carve substrate must be active")
	}

	f := &carveFixture{local: local, ms: realMS, remote: memRBS, syncer: syncer}

	// Store two chunks so the carver packs both into one block.
	h1 := f.storeChunk(t, ctx, bytes.Repeat([]byte("chunk-A"), 512))
	h2 := f.storeChunk(t, ctx, bytes.Repeat([]byte("chunk-B"), 512))

	// First flush: the injected mid-commit failure must roll back the ENTIRE
	// commit and surface an error.
	if err := syncer.carveFlush(ctx, true); err == nil {
		t.Fatal("carveFlush: expected injected commit failure, got nil")
	}

	// Atomicity: nothing from the failed commit is visible — neither chunk is
	// synced (not even h1, whose tx.MarkSynced succeeded before the fault) and
	// no block record exists.
	for _, h := range []block.ContentHash{h1, h2} {
		synced, err := realMS.IsSynced(ctx, h)
		if err != nil {
			t.Fatalf("IsSynced(%s): %v", h, err)
		}
		if synced {
			t.Fatalf("hash %s synced after rolled-back commit — commit not atomic", h)
		}
	}
	records := 0
	if err := realMS.WalkBlockRecords(ctx, func(block.BlockRecord) error {
		records++
		return nil
	}); err != nil {
		t.Fatalf("WalkBlockRecords: %v", err)
	}
	if records != 0 {
		t.Fatalf("block records = %d after rolled-back commit, want 0", records)
	}

	// The batch must be requeued: both chunks still pending.
	if n := syncer.CarvePendingCount(); n != 2 {
		t.Fatalf("carve pending = %d after failed flush, want 2 (batch requeued)", n)
	}

	// Second flush: no more faults — the batch re-carves into a fresh block.
	if err := syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush retry: %v", err)
	}

	// Both chunks synced to the SAME (new) blockID.
	loc1, ok1, err := realMS.GetLocator(ctx, h1)
	if err != nil || !ok1 {
		t.Fatalf("h1: synced=%v err=%v", ok1, err)
	}
	loc2, ok2, err := realMS.GetLocator(ctx, h2)
	if err != nil || !ok2 {
		t.Fatalf("h2: synced=%v err=%v", ok2, err)
	}
	if loc1.BlockID == "" || loc2.BlockID == "" {
		t.Fatalf("chunks must have block locators, got loc1=%+v loc2=%+v", loc1, loc2)
	}
	if loc1.BlockID != loc2.BlockID {
		t.Fatalf("chunks synced to different blocks: %s vs %s", loc1.BlockID, loc2.BlockID)
	}

	// Block record must reflect both chunks.
	rec, exists, err := realMS.GetBlockRecord(ctx, loc1.BlockID)
	if err != nil || !exists {
		t.Fatalf("GetBlockRecord(%s): exists=%v err=%v", loc1.BlockID, exists, err)
	}
	if rec.LiveChunkCount != 2 {
		t.Fatalf("BlockRecord.LiveChunkCount = %d, want 2", rec.LiveChunkCount)
	}

	// Two block objects on remote: the live one plus the failed attempt's
	// orphan (uploaded before the rolled-back commit; block GC reclaims it).
	if got := countRemoteBlocks(t, ctx, mem); got != 2 {
		t.Fatalf("remote blocks = %d, want 2 (live block + pre-commit orphan)", got)
	}
	if got := countRemoteCAS(t, ctx, mem); got != 0 {
		t.Fatalf("remote CAS objects = %d, want 0", got)
	}

	// Carve set fully drained.
	if n := syncer.CarvePendingCount(); n != 0 {
		t.Fatalf("carve pending = %d, want 0", n)
	}

	// Injection must have fired (test was meaningful).
	wrapper.mu.Lock()
	fired := wrapper.count >= wrapper.failOnCall
	wrapper.mu.Unlock()
	if !fired {
		t.Fatal("MarkSynced failure injection did not fire — test setup error")
	}
}

// TestCarve_DispatcherIdleFlush verifies the carve dispatcher (MINOR #2): a
// sub-target write must not produce a block during the size-triggered pass, but
// must appear after the idle window (UploadDelay) elapses. Exercises the
// carveDispatcher timer path that the ManualSync=true fixtures never launch.
func TestCarve_DispatcherIdleFlush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mem := remotememory.New()

	const carveTarget = 4 * 1024 // 4 KiB; test data is 1 KiB — sub-target
	const uploadDelay = 100 * time.Millisecond

	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cfg := DefaultConfig()
	cfg.BlockCarveBytes = carveTarget
	cfg.UploadDelay = uploadDelay
	cfg.ManualSync = false // required: carveDispatcher drives the idle flush

	syncer := NewSyncer(local, mem, ms, cfg)
	syncer.SetSyncedHashStore(ms)
	var memRBS2 remote.RemoteBlockStore = mem
	syncer.SetRemoteBlockStore(memRBS2)

	if !syncer.carveActive.Load() {
		t.Fatal("carve substrate must be active")
	}

	// Launch only the carve dispatcher; no health monitor or legacy dispatcher.
	go syncer.carveDispatcher(ctx)

	// Write 1 KiB — sub-target relative to the 4 KiB carve size.
	f := &carveFixture{local: local, ms: ms, remote: memRBS2, syncer: syncer}
	_ = f.storeChunk(t, ctx, bytes.Repeat([]byte("idle-x"), 170))

	// Size-triggered flush (drainAll=false) must have returned nil: no block yet.
	if got := countRemoteBlocks(t, ctx, mem); got != 0 {
		t.Fatalf("blocks immediately after write = %d, want 0 (idle window not elapsed)", got)
	}

	// Poll until the idle flush fires. Allow 5× the idle window for scheduler
	// jitter; fail if no block appears within that budget.
	deadline := time.Now().Add(5 * uploadDelay)
	for countRemoteBlocks(t, ctx, mem) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("no block after %v; idle flush did not fire", 5*uploadDelay)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if n := syncer.CarvePendingCount(); n != 0 {
		t.Fatalf("carve pending = %d, want 0 after idle flush", n)
	}
}

// TestCarve_UnresolvedChunkRequeuedNotDropped verifies that a carve-claimed
// hash whose bytes are not yet resolvable locally (no log-blob index entry and
// no hash-keyed copy — the pre-Phase-C window a rolled-up chunk passes through,
// since OnChunkComplete registers it for carve before its index commit) is
// LEFT PENDING for a later pass rather than dropped. Dropping such a chunk
// would silently lose its upload (the regression that broke read-after-write
// under concurrent load). A healthy sibling in the same batch still commits.
func TestCarve_UnresolvedChunkRequeuedNotDropped(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	// A healthy chunk that shares the batch with the ghost and must commit.
	data := bytes.Repeat([]byte("survivor-"), 512)
	hReal := f.storeChunk(t, ctx, data)

	// A content hash never stored locally: no log-blob index entry resolves it
	// and no hash-keyed copy exists, so carveChunkBytes reports it transient.
	var ghost block.ContentHash
	ghost[0] = 0x42
	ghost[1] = 0xcd
	const ghostSize int64 = 512
	f.syncer.addPendingHash(ghost, ghostSize)

	if n := f.syncer.CarvePendingCount(); n != 2 {
		t.Fatalf("carve pending = %d, want 2 before flush", n)
	}

	// carveFlush: the ghost is requeued (transient), the survivor is packed and
	// committed. Requeuing is not a flush failure.
	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	// The ghost remains pending (requeued); the survivor left by commit.
	if n := f.syncer.CarvePendingCount(); n != 1 {
		t.Fatalf("carve pending = %d, want 1 after flush (ghost requeued, survivor committed)", n)
	}
	// Exactly one block: the survivor's. No block minted for the ghost.
	if got := countRemoteBlocks(t, ctx, mem); got != 1 {
		t.Fatalf("remote blocks = %d, want 1 (survivor only)", got)
	}
	// The ghost gained no locator — it is still pending, not synced.
	if _, synced, err := f.ms.GetLocator(ctx, ghost); err != nil || synced {
		t.Fatalf("ghost GetLocator: synced=%v err=%v, want unsynced with no error", synced, err)
	}
	// The survivor committed with a block locator and reads back byte-identical.
	loc, synced, err := f.ms.GetLocator(ctx, hReal)
	if err != nil || !synced {
		t.Fatalf("survivor GetLocator: synced=%v err=%v", synced, err)
	}
	if loc.BlockID == "" {
		t.Fatalf("survivor locator has no BlockID: %+v", loc)
	}
	got, err := f.remote.(remote.ChunkReader).ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, hReal)
	if err != nil {
		t.Fatalf("ReadChunk (survivor): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("survivor round-trip mismatch after ghost requeue")
	}
}

// TestCarve_MemoryLocalHashKeyedFallbackPacks pins the hash-keyed fallback in
// carveChunkBytes: a local store WITHOUT the log-blob substrate (memory) has no
// localBlobReader and no log-blob index entries, yet its hash-keyed chunks
// still carve into packed blocks — carve does not require the log-blob tier.
func TestCarve_MemoryLocalHashKeyedFallbackPacks(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local := localmemory.New()
	t.Cleanup(func() { _ = local.Close() })
	mem := remotememory.New()

	cfg := DefaultConfig()
	cfg.ManualSync = true // explicit carveFlush only
	syncer := NewSyncer(local, mem, ms, cfg)
	syncer.SetSyncedHashStore(ms)
	syncer.SetRemoteBlockStore(mem)

	if !syncer.carveActive.Load() {
		t.Fatal("carve must be active for a memory local store (log-blob reader is optional)")
	}

	data := bytes.Repeat([]byte("mem-local-"), 512)
	h := block.ContentHash(blake3.Sum256(data))
	if err := local.Put(ctx, h, data); err != nil {
		t.Fatalf("local Put: %v", err)
	}
	syncer.addPendingHash(h, int64(len(data)))

	if err := syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	if got := countRemoteBlocks(t, ctx, mem); got != 1 {
		t.Fatalf("remote blocks = %d, want 1", got)
	}
	if n := syncer.CarvePendingCount(); n != 0 {
		t.Fatalf("carve pending = %d, want 0 after commit", n)
	}
	loc, synced, err := ms.GetLocator(ctx, h)
	if err != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, err)
	}
	if loc.BlockID == "" {
		t.Fatalf("locator has no BlockID: %+v", loc)
	}
	got, err := mem.ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("memory-local carve round-trip mismatch")
	}
}
