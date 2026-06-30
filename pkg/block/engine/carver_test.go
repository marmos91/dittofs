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
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
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
	local, err := fs.NewWithOptions(t.TempDir(), 0, nil, fs.FSStoreOptions{
		LocalChunkIndex: ms,
	})
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

// TestCarve_NonBlockRemoteDisablesCarve verifies the nil-guard: a remote that
// does not implement RemoteBlockStore leaves carve disabled, so chunks route to
// the legacy mirror set, not the carve set.
func TestCarve_NonBlockRemoteDisablesCarve(t *testing.T) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, nil, fs.FSStoreOptions{LocalChunkIndex: ms})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	// A bare remote that is NOT a RemoteBlockStore.
	bare := nonBlockRemote{remotememory.New()}
	syncer := NewSyncer(local, bare, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)
	syncer.SetRemoteBlockStore(nil) // assertion fails / no block store

	if syncer.carveActive.Load() {
		t.Fatal("carve must be disabled without a RemoteBlockStore")
	}
	var h block.ContentHash
	h[0] = 0x01
	syncer.addPendingHash(h, 100)
	if syncer.CarvePendingCount() != 0 {
		t.Fatal("chunk must not route to carve when carve is disabled")
	}
	if syncer.PendingCount() != 1 {
		t.Fatal("chunk should route to the legacy mirror set when carve is disabled")
	}
}

// failOnNthMarkSynced is a blockCommitter wrapper that injects a MarkSynced
// failure on a specific call (1-indexed). All other methods delegate to the
// embedded real store so the transaction and local-index paths work normally.
// Used by TestCarve_PartialMarkSyncedRetriesSameBlock to simulate a transient
// metadata-store blip after the block record transaction commits.
type failOnNthMarkSynced struct {
	*metadatamemory.MemoryMetadataStore
	mu         sync.Mutex
	count      int
	failOnCall int // which call (1-indexed) to fail
}

func (f *failOnNthMarkSynced) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	f.mu.Lock()
	f.count++
	n := f.count
	f.mu.Unlock()
	if n == f.failOnCall {
		return errors.New("injected MarkSynced failure")
	}
	return f.MemoryMetadataStore.MarkSynced(ctx, hash, loc)
}

// nonBlockRemote wraps a RemoteStore but hides the RemoteBlockStore methods so
// the carve assertion fails.
type nonBlockRemote struct{ remote.RemoteStore }

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

// TestCarve_PartialMarkSyncedRetriesSameBlock is a regression test for
// Finding #1: if DefaultCommitBlock's transaction commits but the MarkSynced
// loop fails on a subsequent chunk, carveAndCommitBlock must retry with the
// SAME blockID (via DefaultCommitBlock's idempotent tx guard) rather than
// minting a new block. After the in-place retry succeeds, exactly one block
// object must exist on the remote, and both chunks must be synced to that
// single block — no orphan record with a non-zero LiveChunkCount and zero
// referencing locators.
func TestCarve_PartialMarkSyncedRetriesSameBlock(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()

	// Wire a real memory store that fails MarkSynced exactly once (call #2) to
	// simulate a transient metadata-store blip after the block record tx commits.
	realMS := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	wrapper := &failOnNthMarkSynced{
		MemoryMetadataStore: realMS,
		failOnCall:          2, // second chunk's MarkSynced fails once
	}

	local, err := fs.NewWithOptions(t.TempDir(), 0, nil, fs.FSStoreOptions{
		LocalChunkIndex: realMS,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cfg := DefaultConfig()
	cfg.BlockCarveBytes = DefaultBlockCarveBytes
	cfg.ManualSync = true

	syncer := NewSyncer(local, mem, realMS, cfg)
	syncer.SetSyncedHashStore(wrapper) // blockCommitter = wrapper (intercepts MarkSynced)
	var memRBS remote.RemoteBlockStore = mem
	syncer.SetRemoteBlockStore(memRBS)

	if !syncer.carveActive.Load() {
		t.Fatal("carve substrate must be active")
	}

	f := &carveFixture{local: local, ms: realMS, remote: memRBS, syncer: syncer}

	// Store two chunks so the carver packs both into one block.
	h1 := f.storeChunk(t, ctx, bytes.Repeat([]byte("chunk-A"), 512))
	h2 := f.storeChunk(t, ctx, bytes.Repeat([]byte("chunk-B"), 512))

	// carveFlush must succeed: the ErrCommitPartial retry completes MarkSynced
	// for h2 using the same blockID.
	if err := syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: expected retry to succeed, got: %v", err)
	}

	// Exactly one block object on remote — no second block minted.
	if got := countRemoteBlocks(t, ctx, mem); got != 1 {
		t.Fatalf("remote blocks = %d, want 1 (no second block after ErrCommitPartial retry)", got)
	}
	if got := countRemoteCAS(t, ctx, mem); got != 0 {
		t.Fatalf("remote CAS objects = %d, want 0", got)
	}

	// Both chunks synced to the SAME blockID.
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
		t.Fatalf("chunks synced to different blocks: %s vs %s — orphan record present",
			loc1.BlockID, loc2.BlockID)
	}

	// Block record must reflect both chunks.
	rec, exists, err := realMS.GetBlockRecord(ctx, loc1.BlockID)
	if err != nil || !exists {
		t.Fatalf("GetBlockRecord(%s): exists=%v err=%v", loc1.BlockID, exists, err)
	}
	if rec.LiveChunkCount != 2 {
		t.Fatalf("BlockRecord.LiveChunkCount = %d, want 2", rec.LiveChunkCount)
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
	local, err := fs.NewWithOptions(t.TempDir(), 0, nil, fs.FSStoreOptions{
		LocalChunkIndex: ms,
	})
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
	for {
		if countRemoteBlocks(t, ctx, mem) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no block after %v; idle flush did not fire", 5*uploadDelay)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if n := syncer.CarvePendingCount(); n != 0 {
		t.Fatalf("carve pending = %d, want 0 after idle flush", n)
	}
}

// TestCarve_RerouteMiss_LegacyPath verifies rerouteCarveMiss (MINOR #4): a
// hash in the carve set but absent from the local log-blob chunk index (a
// stray CAS hash misrouted to carve) is transferred to the legacy standalone-
// mirror pending set without double-counting unsyncedBytes, and the legacy
// upload wake is signalled.
func TestCarve_RerouteMiss_LegacyPath(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	// A content hash NOT stored in the local log-blob / chunk index.
	// addPendingCarveHash forces it into the carve set, simulating a stray CAS
	// hash that was misrouted to carve before the log-blob / CAS distinction
	// was enforced.
	var ghost block.ContentHash
	ghost[0] = 0x42
	ghost[1] = 0xcd
	const ghostSize int64 = 512
	f.syncer.addPendingCarveHash(ghost, ghostSize)

	if n := f.syncer.CarvePendingCount(); n != 1 {
		t.Fatalf("carve pending = %d, want 1 before flush", n)
	}
	if got := f.syncer.UnsyncedBytes(); got != ghostSize {
		t.Fatalf("UnsyncedBytes before flush = %d, want %d", got, ghostSize)
	}

	// carveFlush: GetLocalLocation(ghost) returns (_, false, nil) →
	// rerouteCarveMiss moves ghost to the legacy pending set.
	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	// Ghost must have left the carve set.
	if n := f.syncer.CarvePendingCount(); n != 0 {
		t.Fatalf("carve pending = %d, want 0 after reroute", n)
	}
	// Ghost must be in the legacy pending set (wake signalled by rerouteCarveMiss).
	if n := f.syncer.PendingCount(); n != 1 {
		t.Fatalf("legacy pending = %d, want 1 after reroute", n)
	}
	// No remote block: only a reroute occurred, no commit.
	if got := countRemoteBlocks(t, ctx, mem); got != 0 {
		t.Fatalf("remote blocks = %d, want 0 (reroute only, no commit)", got)
	}
	// unsyncedBytes unchanged: ghost charged once, now in pendingHashes.
	if got := f.syncer.UnsyncedBytes(); got != ghostSize {
		t.Fatalf("UnsyncedBytes after reroute = %d, want %d (charged exactly once)", got, ghostSize)
	}
}
