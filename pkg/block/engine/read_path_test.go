package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/compression"
	"github.com/marmos91/dittofs/pkg/block/encryption"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestReadPath_BlockLocator_Plaintext verifies that fetchResolvedBlock routes a
// block-keyed locator through readChunkVerified and returns the original bytes.
func TestReadPath_BlockLocator_Plaintext(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("read-path-plain-"), 512)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	loc, synced, err := f.ms.GetLocator(ctx, h)
	if err != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, err)
	}
	if loc.IsStandalone() {
		t.Fatalf("expected block locator, got standalone: %+v", loc)
	}

	got, err := f.syncer.fetchResolvedBlock(ctx, &block.FileChunk{Hash: h})
	if err != nil {
		t.Fatalf("fetchResolvedBlock: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("fetchResolvedBlock round-trip mismatch (plaintext)")
	}
}

// TestReadPath_BlockLocator_ThroughCompress verifies that fetchResolvedBlock
// correctly reads back a chunk that was carved through a compressed remote.
func TestReadPath_BlockLocator_ThroughCompress(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()
	dec, err := compression.NewRemote(base, compression.CompressionPolicy{Algo: compression.AlgoZstd})
	if err != nil {
		t.Fatalf("compression.NewRemote: %v", err)
	}
	f := newCarveFixture(t, dec, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("XXXX-YYYY-ZZZZ-"), 4096)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	loc, synced, lerr := f.ms.GetLocator(ctx, h)
	if lerr != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, lerr)
	}
	if loc.IsStandalone() {
		t.Fatalf("expected block locator, got standalone: %+v", loc)
	}

	got, err := f.syncer.fetchResolvedBlock(ctx, &block.FileChunk{Hash: h})
	if err != nil {
		t.Fatalf("fetchResolvedBlock (compress): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("fetchResolvedBlock round-trip mismatch (compressed remote)")
	}
}

// TestReadPath_BlockLocator_ThroughCompressEncrypt verifies that fetchResolvedBlock
// correctly reads back a chunk carved through a compression(encryption(base)) stack.
func TestReadPath_BlockLocator_ThroughCompressEncrypt(t *testing.T) {
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

	data := bytes.Repeat([]byte("secret-read-path-"), 1024)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	loc, synced, lerr := f.ms.GetLocator(ctx, h)
	if lerr != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, lerr)
	}
	if loc.IsStandalone() {
		t.Fatalf("expected block locator, got standalone: %+v", loc)
	}

	got, err := f.syncer.fetchResolvedBlock(ctx, &block.FileChunk{Hash: h})
	if err != nil {
		t.Fatalf("fetchResolvedBlock (compress+encrypt): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("fetchResolvedBlock round-trip mismatch (compress+encrypt remote)")
	}
}

// TestReadPath_CASBackCompat_StandaloneLocator verifies that a hash with no
// recorded locator (standalone default) is read back through the CAS path.
func TestReadPath_CASBackCompat_StandaloneLocator(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()

	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, nil, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	syncer := NewSyncer(local, mem, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)

	data := []byte("cas-back-compat-payload")
	h := block.ContentHash(blake3.Sum256(data))

	// Write directly to the CAS remote (simulates a pre-#1414 standalone upload).
	if err := mem.Put(ctx, h, data); err != nil {
		t.Fatalf("mem.Put: %v", err)
	}
	// Record a standalone locator (BlockID=="" → CAS path).
	if err := ms.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	got, err := syncer.fetchResolvedBlock(ctx, &block.FileChunk{Hash: h})
	if err != nil {
		t.Fatalf("fetchResolvedBlock (CAS back-compat): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("fetchResolvedBlock round-trip mismatch (CAS standalone)")
	}
}

// TestReadPath_CorruptBlock_FailClosed verifies that readChunkVerified returns
// ErrCASContentMismatch when the chunk is read back with the wrong expected hash.
func TestReadPath_CorruptBlock_FailClosed(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()
	f := newCarveFixture(t, mem, DefaultBlockCarveBytes)

	data := bytes.Repeat([]byte("corrupt-check-data-"), 256)
	h := f.storeChunk(t, ctx, data)

	if err := f.syncer.carveFlush(ctx, true); err != nil {
		t.Fatalf("carveFlush: %v", err)
	}

	loc, synced, err := f.ms.GetLocator(ctx, h)
	if err != nil || !synced {
		t.Fatalf("GetLocator: synced=%v err=%v", synced, err)
	}
	if loc.IsStandalone() {
		t.Fatalf("expected block locator, got standalone: %+v", loc)
	}

	// Build a wrong hash by flipping every byte of h.
	var wrongHash block.ContentHash
	for i, b := range h {
		wrongHash[i] = ^b
	}

	_, err = f.syncer.readChunkVerified(ctx, loc, wrongHash)
	if !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("readChunkVerified with wrong hash: want ErrCASContentMismatch, got %v", err)
	}
}

// noChunkReaderRemote wraps a RemoteStore but hides the ChunkReader interface,
// so readChunkVerified must return ErrChunkReadUnsupported.
type noChunkReaderRemote struct{ remote.RemoteStore }

// TestReadPath_NoChunkReader_FailsClosed verifies that readChunkVerified returns
// ErrChunkReadUnsupported when the remote store does not implement ChunkReader.
func TestReadPath_NoChunkReader_FailsClosed(t *testing.T) {
	ctx := context.Background()
	base := remotememory.New()

	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, nil, fs.FSStoreOptions{LocalChunkIndex: ms})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	// Wrap the remote in a type that hides ChunkReader.
	stripped := noChunkReaderRemote{base}
	syncer := NewSyncer(local, stripped, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)
	// Do NOT wire a RemoteBlockStore: carve stays disabled, which is fine —
	// we call readChunkVerified directly.

	data := []byte("no-chunk-reader-payload")
	h := block.ContentHash(blake3.Sum256(data))

	// Record a block locator for h — not standalone — to force the block-read branch.
	blockLoc := block.ChunkLocator{BlockID: "fake-block", WireOffset: 10, WireLength: 20}
	if err := ms.MarkSynced(ctx, h, blockLoc); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	_, err = syncer.readChunkVerified(ctx, blockLoc, h)
	if !errors.Is(err, remote.ErrChunkReadUnsupported) {
		t.Fatalf("readChunkVerified without ChunkReader: want ErrChunkReadUnsupported, got %v", err)
	}
}
