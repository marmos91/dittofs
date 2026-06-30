package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
// through a compression(encryption(base)) remote seals each chunk (the block's
// wire body is neither the plaintext nor merely compressed) and decodes back
// byte-identical through the decorated remote's ReadChunk — proving no plaintext
// at rest on an encrypted share.
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

	// Inspect the raw block bytes on the BASE store: the chunk's wire body must
	// not contain the plaintext (it is encrypted), confirming sealing happened.
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
