package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/compression"
	"github.com/marmos91/dittofs/pkg/block/encryption"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
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

// TestReadPath_StandaloneLocatorRefused pins the post-#1493 inversion of the
// old CAS back-compat contract: a synced hash whose recorded locator is still
// standalone (BlockID == "") is post-migration drift — the one-shot startup
// migration repacked every legacy standalone chunk into a block, so
// fetchResolvedBlock must refuse the read (no silent zeros, no legacy
// fallback), even though the legacy cas/ object still exists on the remote.
// The positive round-trip contract lives in TestReadPath_BlockLocator_Plaintext
// above, which seeds through the carve path and reads back byte-identical.
func TestReadPath_StandaloneLocatorRefused(t *testing.T) {
	ctx := context.Background()
	mem := remotememory.New()

	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, nil, fs.FSStoreOptions{
		LocalChunkIndex: metadatamemory.NewMemoryMetadataStoreWithDefaults()})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	syncer := NewSyncer(local, mem, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)

	data := []byte("cas-back-compat-payload")
	h := block.ContentHash(blake3.Sum256(data))

	// Plant the legacy standalone cas/ object (a pre-#1414 upload shape) so
	// the refusal below is provably fail-closed policy, not a missing object.
	if err := mem.Put(ctx, h, data); err != nil {
		t.Fatalf("mem.Put: %v", err)
	}
	// Record a standalone locator (BlockID == "") — post-migration drift.
	if err := ms.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	got, err := syncer.fetchResolvedBlock(ctx, &block.FileChunk{ID: "share/standalone/0", Hash: h})
	if err == nil {
		t.Fatalf("fetchResolvedBlock: want post-migration drift refusal, got nil (data=%d bytes)", len(got))
	}
	if !strings.Contains(err.Error(), "post-migration drift") {
		t.Fatalf("fetchResolvedBlock err = %v, want post-migration drift refusal", err)
	}
	if got != nil {
		t.Fatalf("fetchResolvedBlock data = %d bytes, want nil on refusal", len(got))
	}
}

// TestReadPath_CorruptBlock_FailClosed verifies that readChunkVerified returns
// ErrChunkContentMismatch when the chunk is read back with the wrong expected hash.
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
	if !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("readChunkVerified with wrong hash: want ErrChunkContentMismatch, got %v", err)
	}
}
